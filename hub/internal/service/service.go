// Package service implements hub.Service over Hermes gateway connections,
// with per-user authorization of every backend.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/google/uuid"
)

// Directory returns the groups a user had at their latest login.
type Directory interface {
	Groups(ctx context.Context, user string) ([]string, error)
}

// Dialer opens gateway connections to one backend.
type Dialer interface {
	Dial(ctx context.Context, onEvent func(gateway.Event)) (*gateway.Client, error)
}

// Options tune a Service. Zero values pick the defaults.
type Options struct {
	// IdleDetach is how long a session stays subscribed with no viewer.
	IdleDetach time.Duration
	// RingSize is how many events per session the hub keeps for viewers
	// that reconnect.
	RingSize int
	// ViewerBuffer is each viewer's channel size. A viewer that falls this
	// far behind is dropped and must attach again.
	ViewerBuffer int
	// CallTimeout bounds each gateway request.
	CallTimeout time.Duration
	// Dialer builds the dialer of a backend. Default: gateway.NewDialer.
	Dialer func(config.Backend) Dialer
	Logger *slog.Logger
	// HoldTimeout is how long a prompt waits for a restarting or
	// unreachable backend before the hub gives up on it. Default 10m.
	HoldTimeout time.Duration
	// HoldRetryMin and HoldRetryMax bound the backoff between attempts to
	// send held prompts. Defaults 250ms and 5s.
	HoldRetryMin, HoldRetryMax time.Duration
	// Relay carries held-prompt events to the other replicas. Nil for a
	// single replica.
	Relay Relay
	// PDF reads attached PDFs, so the agent gets their text and first
	// pages. Nil: a PDF is a plain file.
	PDF PDFReader
	// Answers records the answers to approvals that pass through the hub,
	// so every replica can say which choice answered one. Nil: the hub
	// works every outcome out from the tool call.
	Answers hub.AnswerStore
	// SteerRetry is how long a steer waits before it tries again while the
	// running turn has no agent to take it, and SteerWindow how long it
	// keeps trying. Defaults 250ms and 3s.
	SteerRetry, SteerWindow time.Duration
	// SteerSettle is how long StopAndSend waits for a stopped turn to give
	// back the steer it held.
	SteerSettle time.Duration
	// CommandsTTL is how long the hub keeps a backend's command catalog.
	// Default 5m.
	CommandsTTL time.Duration
}

// Service is the hub core.
type Service struct {
	backends map[string]*backend
	order    []string
	dir      Directory
	opts     Options
	log      *slog.Logger
	origin   string
	relayOut chan []byte
	// relayPending counts relay events queued but not yet published.
	relayPending atomic.Int64
	stop         context.CancelFunc
	// files places attached files on devices. Nil without devices.
	files hub.SessionFiles
	// uploads counts the prompts with attachments being staged.
	uploads chan struct{}
	// notify takes the notices of every subscribed session.
	notify atomic.Pointer[func(hub.Notice)]
	// turnErrors takes the failed turns of every subscribed session.
	turnErrors atomic.Pointer[func(TurnError)]
}

// SetNotify makes the service hand each notice to f. f must not block: it
// runs while a session's lock is held.
func (s *Service) SetNotify(f func(hub.Notice)) { s.notify.Store(&f) }

func (s *Service) notice(n hub.Notice) {
	if f := s.notify.Load(); f != nil {
		(*f)(n)
	}
}

var _ hub.Service = (*Service)(nil)

// New returns a Service for the configured backends.
func New(cfgs []config.Backend, dir Directory, opts Options) *Service {
	if opts.IdleDetach == 0 {
		opts.IdleDetach = 5 * time.Minute
	}
	if opts.RingSize == 0 {
		opts.RingSize = 20000
	}
	if opts.ViewerBuffer == 0 {
		opts.ViewerBuffer = 4096
	}
	if opts.CallTimeout == 0 {
		opts.CallTimeout = 30 * time.Second
	}
	if opts.Dialer == nil {
		opts.Dialer = func(b config.Backend) Dialer {
			return gateway.NewDialer(gateway.Credentials{BaseURL: b.URL, Provider: b.Provider, Username: b.Username, Password: b.Password})
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HoldTimeout == 0 {
		opts.HoldTimeout = 10 * time.Minute
	}
	if opts.HoldRetryMin == 0 {
		opts.HoldRetryMin = 250 * time.Millisecond
	}
	if opts.HoldRetryMax == 0 {
		opts.HoldRetryMax = 5 * time.Second
	}
	if opts.SteerRetry == 0 {
		opts.SteerRetry = 250 * time.Millisecond
	}
	if opts.SteerSettle == 0 {
		opts.SteerSettle = 15 * time.Second
	}
	if opts.SteerWindow == 0 {
		opts.SteerWindow = 3 * time.Second
	}
	if opts.CommandsTTL == 0 {
		opts.CommandsTTL = 5 * time.Minute
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &Service{backends: map[string]*backend{}, dir: dir, opts: opts, log: opts.Logger, origin: uuid.NewString(), stop: stop,
		uploads: make(chan struct{}, maxUploads)}
	for _, c := range cfgs {
		b := &backend{svc: s, cfg: c, dialer: opts.Dialer(c), sessions: map[string]*live{},
			log: s.log.With("backend", c.ID), held: map[string][]*heldPrompt{}, remote: map[string][]hub.HeldInput{},
			kick: make(chan struct{}, 1), sending: map[string]*sessionGate{}, obs: newObserver(time.Now())}
		s.backends[c.ID] = b
		s.order = append(s.order, c.ID)
	}
	if opts.Relay != nil {
		s.relayOut = make(chan []byte, 256)
		go s.relay(ctx, opts.Relay)
	}
	return s
}

// Close drops every gateway connection. Sessions stay owned by the gateways.
func (s *Service) Close() {
	if s.relayOut != nil {
		s.flushRelay()
	}
	s.stop()
	for _, b := range s.backends {
		b.close()
	}
}

func (s *Service) allowed(ctx context.Context, user string, b *backend) (bool, error) {
	if user == "" {
		return false, nil
	}
	if slices.Contains(b.cfg.Users, user) {
		return true, nil
	}
	if len(b.cfg.Groups) == 0 || s.dir == nil {
		return false, nil
	}
	groups, err := s.dir.Groups(ctx, user)
	if err != nil {
		return false, fmt.Errorf("look up groups: %w", err)
	}
	for _, g := range groups {
		if slices.Contains(b.cfg.Groups, g) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) backend(ctx context.Context, user, id string) (*backend, error) {
	b, ok := s.backends[id]
	if !ok {
		return nil, hub.ErrUnknownBackend
	}
	ok, err := s.allowed(ctx, user, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, hub.ErrForbidden
	}
	return b, nil
}

// Allowed fails with hub.ErrUnknownBackend or hub.ErrForbidden unless user
// may use the backend.
func (s *Service) Allowed(ctx context.Context, user, backendID string) error {
	_, err := s.backend(ctx, user, backendID)
	return err
}

// Backends lists the backends user may use.
func (s *Service) Backends(ctx context.Context, user string) ([]hub.Backend, error) {
	out := []hub.Backend{}
	for _, id := range s.order {
		b := s.backends[id]
		ok, err := s.allowed(ctx, user, b)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, hub.Backend{ID: b.cfg.ID, Name: b.cfg.Name})
		}
	}
	return out, nil
}

// BackendStatus is the last connection error of each of user's backends
// ("" when the last attempt worked).
func (s *Service) BackendStatus(ctx context.Context, user string) (map[string]string, error) {
	bs, err := s.Backends(ctx, user)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, hb := range bs {
		out[hb.ID] = s.backends[hb.ID].lastError()
	}
	return out, nil
}

// Sessions lists live and stored sessions on all of user's backends, newest
// first. A backend that cannot be reached is left out; BackendStatus says why.
func (s *Service) Sessions(ctx context.Context, user string) ([]hub.Session, error) {
	bs, err := s.Backends(ctx, user)
	if err != nil {
		return nil, err
	}
	var (
		mu  sync.Mutex
		out = []hub.Session{}
		wg  sync.WaitGroup
	)
	for _, hb := range bs {
		b := s.backends[hb.ID]
		wg.Go(func() {
			list, err := b.list(ctx)
			if err != nil {
				b.log.Warn("list sessions", "error", err.Error())
				if list = b.lastList(err); list == nil {
					return
				}
			}
			mu.Lock()
			out = append(out, list...)
			mu.Unlock()
		})
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// NewSession starts a session on a backend and subscribes the hub to it.
func (s *Service) NewSession(ctx context.Context, user, backendID string) (hub.Session, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.Session{}, err
	}
	return b.create(ctx, b.cfg.CWD, "")
}

// NewSessionAt starts a session whose tools run in cwd on the backend's
// server and whose row records rowCWD, as for a session that runs on a
// device: cwd is the device plugin's placeholder and rowCWD the device
// directory.
func (s *Service) NewSessionAt(ctx context.Context, user, backendID, cwd, rowCWD string) (hub.Session, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.Session{}, err
	}
	return b.create(ctx, cwd, rowCWD)
}

// Attach streams a session's events. See hub.Service.
func (s *Service) Attach(ctx context.Context, user, backendID, sessionID string, sinceSeq int64) (<-chan hub.Event, func(), error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, nil, err
	}
	ls, offline, err := b.attachable(ctx, sessionID)
	if err != nil {
		return nil, nil, refusal(err)
	}
	ch, cancel := ls.attach(sinceSeq)
	if offline {
		ls.status(false)
		b.retry()
	}
	ls.unpin()
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ch, cancel, nil
}

// Prompt submits text as the next turn of a session. When the backend is
// restarting or cannot be reached, it holds the prompt and sends it, under
// the same submission ID, once the backend is back. A prompt behind held
// ones is held too, to keep their order. Attachments need the backend: a
// prompt with files is refused while it is away. A slash command runs
// through slash.exec (see command). With hub.WithSteer, a prompt that
// queues behind the running turn is handed to that turn (see steer).
func (s *Service) Prompt(ctx context.Context, user, backendID, sessionID, text string, files ...hub.Attachment) (hub.PromptResult, error) {
	if err := hub.CheckAttachments(files); err != nil {
		return hub.PromptResult{}, err
	}
	if len(files) > 0 && strings.HasPrefix(strings.TrimSpace(text), "/") {
		return hub.PromptResult{}, fmt.Errorf("%w: Send a slash command without attachments", hub.ErrAttachment)
	}
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.PromptResult{}, err
	}
	id := hub.SubmissionID(ctx)
	if id == "" {
		id = hub.NewSubmissionID()
	}
	if _, _, ok := hub.SlashCommand(text); ok {
		return s.command(ctx, b, user, sessionID, id, text)
	}
	release := b.gate(sessionID)
	res, receipt, err := s.promptLocked(ctx, b, user, sessionID, id, text, files)
	release()
	if err != nil || !hub.Steer(ctx) || len(files) > 0 || res.Held || (receipt.Status != gateway.AdmissionQueued && receipt.Status != gateway.AdmissionStarted) {
		return res, err
	}
	res.Steered = b.steerOrFind(context.WithoutCancel(ctx), sessionID, receipt)
	return res, nil
}

// promptLocked is Prompt with the session's gate held. The receipt is the
// gateway's answer to a direct submit; it is empty for a held prompt.
func (s *Service) promptLocked(ctx context.Context, b *backend, user, sessionID, id, text string,
	files []hub.Attachment) (hub.PromptResult, gateway.Receipt, error) {
	if b.heldCount(sessionID) > 0 {
		if len(files) > 0 {
			return hub.PromptResult{}, gateway.Receipt{}, errAway
		}
		res, err := b.hold(user, sessionID, id, text, nil, false)
		return res, gateway.Receipt{}, err
	}
	var (
		atts   []gateway.Attachment
		notice string
		err    error
	)
	if len(files) > 0 {
		text, atts, notice, err = b.stage(ctx, user, sessionID, text, files)
		if err != nil {
			return hub.PromptResult{}, gateway.Receipt{}, err
		}
	}
	receipt, err := b.submit(ctx, sessionID, id, text, atts)
	if err == nil {
		return hub.PromptResult{ID: id, Notice: notice}, receipt, nil
	}
	if !gateway.Retryable(err) {
		return hub.PromptResult{}, gateway.Receipt{}, refusal(err)
	}
	b.log.Info("backend unavailable, holding a prompt", "session", sessionID, "error", err.Error())
	res, err := b.hold(user, sessionID, id, text, atts, errors.Is(err, gateway.ErrClosed))
	res.Notice = notice
	return res, gateway.Receipt{}, err
}

// submit sends one prompt.submit. Hermes answers a repeated submission ID
// with the admission it already has, so a retry never runs a prompt twice.
func (b *backend) submit(ctx context.Context, sessionID, id, text string, atts []gateway.Attachment) (gateway.Receipt, error) {
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return gateway.Receipt{}, err
	}
	defer ls.unpin()
	var receipt gateway.Receipt
	err = b.call(ctx, ls, gateway.MethodPromptSubmit,
		gateway.SubmitParams{SessionID: ls.id, Text: text, SubmissionID: id, Attachments: atts}, &receipt)
	return receipt, err
}

// Interrupt stops the session's running turn.
func (s *Service) Interrupt(ctx context.Context, user, backendID, sessionID string) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return refusal(err)
	}
	defer ls.unpin()
	ls.mu.Lock()
	gen := ls.generation
	ls.mu.Unlock()
	return b.call(ctx, ls, gateway.MethodInterrupt, gateway.InterruptParams{SessionID: ls.id, ExecutionGeneration: gen}, nil)
}

// RespondApproval answers a pending approval: allow runs it once, deny
// refuses it.
func (s *Service) RespondApproval(ctx context.Context, user, backendID, sessionID, approvalID string, allow bool) error {
	choice := gateway.ChoiceDeny
	if allow {
		choice = gateway.ChoiceOnce
	}
	return s.AnswerPrompt(ctx, user, backendID, sessionID, approvalID, hub.Answer{Choice: choice})
}

// PendingPrompt returns a prompt of the session that still waits for an
// answer.
func (s *Service) PendingPrompt(ctx context.Context, user, backendID, sessionID, promptID string) (hub.PromptInfo, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.PromptInfo{}, err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return hub.PromptInfo{}, refusal(err)
	}
	defer ls.unpin()
	ls.mu.Lock()
	p, ok := ls.prompts[promptID]
	ls.mu.Unlock()
	if !ok {
		return hub.PromptInfo{}, hub.ErrUnknownApproval
	}
	return promptInfo(p), nil
}

// maxAnswer is the longest answer the gateway takes.
const maxAnswer = 16384

// AnswerPrompt answers a pending approval with one of its choices, or a
// question with text, picked choices or a skip.
func (s *Service) AnswerPrompt(ctx context.Context, user, backendID, sessionID, promptID string, a hub.Answer) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return refusal(err)
	}
	defer ls.unpin()
	ls.mu.Lock()
	p, ok := ls.prompts[promptID]
	ls.mu.Unlock()
	if !ok {
		return hub.ErrUnknownApproval
	}
	var (
		method string
		params any
	)
	switch p.Kind {
	case gateway.PromptApproval:
		if !slices.Contains(p.Choices, a.Choice) {
			return fmt.Errorf("%w: this approval does not offer %q", hub.ErrBadValue, a.Choice)
		}
		method, params = gateway.MethodApproval, gateway.ApprovalParams{SessionID: ls.id, ExecutionGeneration: p.ExecutionGeneration,
			PromptID: p.PromptID, Choice: a.Choice}
	case gateway.PromptClarify:
		answer, err := clarifyAnswer(p, a)
		if err != nil {
			return err
		}
		method, params = gateway.MethodClarify, gateway.ClarifyParams{SessionID: ls.id, ExecutionGeneration: p.ExecutionGeneration,
			PromptID: p.PromptID, Answer: answer}
	default:
		return hub.ErrUnknownApproval
	}
	answer := hub.PromptAnswer{Choice: a.Choice, Via: hub.AnsweredVia(ctx)}
	recorded := p.Kind == gateway.PromptApproval && s.recordAnswer(ctx, backendID, promptID, answer)
	var res struct {
		Status string `json:"status"`
	}
	err = b.call(ctx, ls, method, params, &res)
	resolved := err == nil && res.Status == "already_resolved"
	if (err != nil || resolved) && recorded {
		s.forgetAnswer(ctx, backendID, promptID, answer)
	}
	if resolved {
		ls.mu.Lock()
		ls.settled(promptID)
		ls.mu.Unlock()
		return hub.ErrUnknownApproval
	}
	return err
}

// answerTimeout bounds recording an answer, so a slow store does not hold
// up the answer for long.
const answerTimeout = 2 * time.Second

// recordAnswer records an answer before it goes to Hermes, so the replica
// that sees the approval settle can tell which choice answered it. It
// reports whether it recorded this one: false also when the approval
// already had an answer.
func (s *Service) recordAnswer(ctx context.Context, backendID, promptID string, a hub.PromptAnswer) bool {
	if s.opts.Answers == nil {
		return false
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), answerTimeout)
	defer cancel()
	recorded, err := s.opts.Answers.RecordAnswer(cctx, backendID, promptID, a)
	if err != nil {
		s.log.Warn("Failed to record an approval's answer", "backend", backendID, "error", err.Error())
	}
	return recorded
}

// forgetAnswer drops a recorded answer that Hermes did not take.
func (s *Service) forgetAnswer(ctx context.Context, backendID, promptID string, a hub.PromptAnswer) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), answerTimeout)
	defer cancel()
	if err := s.opts.Answers.ForgetAnswer(cctx, backendID, promptID, a); err != nil {
		s.log.Warn("Failed to forget an approval's answer", "backend", backendID, "error", err.Error())
	}
}

// clarifyAnswer is the answer clarify.respond takes for a: "" to skip, a
// JSON array of labels for picks, or the text.
func clarifyAnswer(p gateway.Prompt, a hub.Answer) (string, error) {
	switch {
	case a.Skip:
		return "", nil
	case len(a.Picks) > 0:
		if !p.MultiSelect && len(a.Picks) > 1 {
			return "", fmt.Errorf("%w: pick one answer", hub.ErrBadValue)
		}
		for _, pick := range a.Picks {
			if !slices.Contains(p.Choices, pick) {
				return "", fmt.Errorf("%w: that is not one of the answers", hub.ErrBadValue)
			}
		}
		if !p.MultiSelect {
			return a.Picks[0], nil
		}
		out, err := json.Marshal(a.Picks)
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return "", fmt.Errorf("%w: the answer is empty", hub.ErrBadValue)
	}
	if len(text) > maxAnswer {
		return "", fmt.Errorf("%w: the answer is too long", hub.ErrBadValue)
	}
	return text, nil
}

// CancelInput drops a queued input before it runs.
func (s *Service) CancelInput(ctx context.Context, user, backendID, sessionID, inputID string) error {
	return s.input(ctx, user, backendID, sessionID, inputID, gateway.AdmissionQueued, func(ls *live, _ gateway.PendingAdmission) (string, any) {
		return gateway.MethodPromptCancel, gateway.AdmissionParams{SessionID: ls.id, AdmissionID: inputID}
	})
}

// DismissLost resolves an input whose turn a backend restart cut off. The
// gateway never re-runs it; the inputs queued after it then run in order.
func (s *Service) DismissLost(ctx context.Context, user, backendID, sessionID, inputID string) error {
	return s.input(ctx, user, backendID, sessionID, inputID, gateway.AdmissionUnknown, func(ls *live, p gateway.PendingAdmission) (string, any) {
		var gen int64
		if p.ExecutionGeneration != nil {
			gen = *p.ExecutionGeneration
		}
		return gateway.MethodResolveLost, gateway.ResolveLostParams{SessionID: ls.id, AdmissionID: inputID, ExecutionGeneration: gen}
	})
}

// input runs one call on a pending input in the given status, then takes a
// fresh snapshot so every viewer sees the new pending list.
func (s *Service) input(ctx context.Context, user, backendID, sessionID, inputID, status string,
	req func(*live, gateway.PendingAdmission) (string, any)) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return refusal(err)
	}
	defer ls.unpin()
	ls.mu.Lock()
	p, ok := ls.inputs[inputID]
	ls.mu.Unlock()
	if !ok || p.Status != status {
		return hub.ErrUnknownInput
	}
	method, params := req(ls, p)
	if err := b.call(ctx, ls, method, params, nil); err != nil {
		return err
	}
	ls.mu.Lock()
	ls.resnapshotLocked()
	ls.mu.Unlock()
	return nil
}

type backend struct {
	svc    *Service
	cfg    config.Backend
	dialer Dialer
	log    *slog.Logger

	dialMu   sync.Mutex
	mu       sync.Mutex
	client   *gateway.Client
	lastErr  string
	sessions map[string]*live
	closed   bool
	retrying bool

	// holdMu guards the held prompts. It is taken before ls.mu, never
	// after, and events about held prompts are sent while it is held.
	holdMu   sync.Mutex
	held     map[string][]*heldPrompt
	remote   map[string][]hub.HeldInput
	flushing bool
	draining bool
	kick     chan struct{}

	// gateMu guards sending: a gate per session that orders direct
	// submits with the check for held prompts.
	gateMu  sync.Mutex
	sending map[string]*sessionGate

	// obs keeps what the backend's usage events said.
	obs *observer

	// cmds is the backend's command catalog as the hub runs it, read at
	// cmdsAt.
	cmdMu  sync.Mutex
	cmds   []hub.Command
	cmdsAt time.Time

	// listMu guards the last session list that worked, served while the
	// backend restarts.
	listMu   sync.Mutex
	listed   []hub.Session
	listedAt time.Time

	// capsMu guards caps, what runtime.describe listed on capsOf.
	capsMu sync.Mutex
	capsOf *gateway.Client
	caps   []string
}

// listStaleAfter bounds how old a session list may be to stand in for a
// backend that is restarting.
const listStaleAfter = 15 * time.Minute

// lastList is the last session list that worked, when err says the backend
// is only restarting and that list is recent; else nil.
func (b *backend) lastList(err error) []hub.Session {
	if !gateway.Retryable(err) {
		return nil
	}
	b.listMu.Lock()
	defer b.listMu.Unlock()
	if b.listed == nil || time.Since(b.listedAt) > listStaleAfter {
		return nil
	}
	return slices.Clone(b.listed)
}

type sessionGate struct {
	mu   sync.Mutex
	refs int
}

// gate serializes direct submits to one session, so a prompt that is held
// after a slow failure stays ahead of the prompts sent after it. The
// returned func releases the gate.
func (b *backend) gate(sessionID string) func() {
	b.gateMu.Lock()
	g := b.sending[sessionID]
	if g == nil {
		g = &sessionGate{}
		b.sending[sessionID] = g
	}
	g.refs++
	b.gateMu.Unlock()
	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		b.gateMu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(b.sending, sessionID)
		}
		b.gateMu.Unlock()
	}
}

func (b *backend) lastError() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

func (b *backend) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *backend) close() {
	b.mu.Lock()
	b.closed = true
	c := b.client
	b.client = nil
	b.mu.Unlock()
	if c != nil {
		c.Close()
	}
	b.wake()
}

func (b *backend) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), b.svc.opts.CallTimeout)
}

func alive(c *gateway.Client) bool {
	if c == nil {
		return false
	}
	select {
	case <-c.Done():
		return false
	default:
		return true
	}
}

var (
	errClosed  = errors.New("service closed")
	errRetired = errors.New("session retired while subscribing")
)

func (b *backend) conn(ctx context.Context) (*gateway.Client, error) {
	b.dialMu.Lock()
	defer b.dialMu.Unlock()
	b.mu.Lock()
	c, closed := b.client, b.closed
	b.mu.Unlock()
	if closed {
		return nil, errClosed
	}
	if alive(c) {
		return c, nil
	}
	dctx, cancel := b.timeout(ctx)
	defer cancel()
	c, err := b.dialer.Dial(dctx, b.onEvent)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.lastErr = err.Error()
		return nil, err
	}
	if b.closed {
		c.Close()
		return nil, errClosed
	}
	b.client, b.lastErr = c, ""
	go b.watch(c)
	b.wake()
	return c, nil
}

func (b *backend) watch(c *gateway.Client) {
	<-c.Done()
	b.mu.Lock()
	if b.client == c {
		b.client = nil
		if c.Err() != nil && !b.closed {
			b.lastErr = c.Err().Error()
		}
	}
	sessions := make([]*live, 0, len(b.sessions))
	for _, ls := range b.sessions {
		if !slices.Contains(sessions, ls) {
			sessions = append(sessions, ls)
		}
	}
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return
	}
	b.log.Warn("gateway connection lost", "error", fmt.Sprint(c.Err()))
	offline := func() {
		for _, ls := range sessions {
			if ls.on(c) {
				ls.status(false)
			}
		}
	}
	if gateway.RestartClose(c.Err()) {
		// A gateway that closed for a restart is replaced within seconds: re-subscribe first and
		// tell viewers only when that takes longer than restartReconnectGrace.
		time.AfterFunc(restartReconnectGrace, offline)
	} else {
		offline()
	}
	b.retry()
}

// restartReconnectGrace is how long sessions of a gateway that closed for a
// restart stay shown as connected while the hub re-subscribes them.
const restartReconnectGrace = 10 * time.Second

// retry reconnects and re-subscribes every session that has a viewer,
// with backoff, until that works or no viewer is left. It outlives any
// request.
//
//nolint:contextcheck // see above
func (b *backend) retry() {
	b.mu.Lock()
	if b.retrying || b.closed {
		b.mu.Unlock()
		return
	}
	b.retrying = true
	b.mu.Unlock()
	go func() {
		defer func() {
			b.mu.Lock()
			b.retrying = false
			b.mu.Unlock()
		}()
		delay := 500 * time.Millisecond
		for {
			b.mu.Lock()
			var watched []*live
			for _, ls := range b.sessions {
				if ls.viewerCount() > 0 && !slices.Contains(watched, ls) {
					watched = append(watched, ls)
				}
			}
			closed := b.closed
			b.mu.Unlock()
			if closed || len(watched) == 0 {
				return
			}
			failed := false
			for _, ls := range watched {
				_, err := b.ensure(context.Background(), ls)
				var rerr *gateway.RPCError
				switch {
				case errors.As(err, &rerr):
					b.log.Warn("re-subscribe refused, giving up on session", "session", ls.id, "error", err.Error())
					ls.fail(err)
				case err != nil:
					b.log.Warn("re-subscribe", "session", ls.id, "error", err.Error())
					failed = true
				case !ls.onAlive():
					failed = true
				}
			}
			if !failed {
				return
			}
			time.Sleep(delay)
			delay = min(delay*2, 30*time.Second)
		}
	}()
}

func (b *backend) onEvent(ev gateway.Event) {
	if ev.SessionID == "" {
		return
	}
	b.mu.Lock()
	ls := b.sessions[ev.SessionID]
	b.mu.Unlock()
	if ls != nil {
		ls.handle(ev)
	}
}

func (b *backend) list(ctx context.Context) ([]hub.Session, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	rows := map[string]hub.Session{}
	// The live list with include_hidden adds archived (and hidden)
	// sessions; the plain live list that follows clears the flag again.
	lists := []gateway.ListParams{{Scope: gateway.ScopeStored, Limit: 200, Picker: true},
		{Scope: gateway.ScopeLive, Limit: 200, IncludeHidden: true, Picker: true}, {Scope: gateway.ScopeLive, Limit: 200, Picker: true}}
	for _, lp := range lists {
		var res gateway.ListResult
		if err := c.Call(cctx, gateway.MethodSessionList, lp, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Sessions {
			if hub.IsBackground(r.SessionID) {
				continue
			}
			s := hub.Session{ID: r.SessionID, BackendID: b.cfg.ID, Title: r.Title, Live: lp.Scope == gateway.ScopeLive,
				Running: r.Running, MessageCount: r.MessageCount, Archived: lp.IncludeHidden}
			if r.Source != nil {
				s.Source = *r.Source
			}
			switch {
			case r.LastActiveAt != nil && *r.LastActiveAt > 0:
				s.UpdatedAt = time.UnixMilli(int64(*r.LastActiveAt * 1000))
			case r.StartedAt != nil:
				s.UpdatedAt = time.UnixMilli(int64(*r.StartedAt * 1000))
			}
			rows[s.ID] = s
		}
	}
	var roster gateway.Roster
	if err := c.Call(cctx, gateway.MethodSubagentRoster, map[string]any{}, &roster); err != nil {
		b.log.Debug("subagent.roster", "error", err.Error())
	}
	for _, r := range roster.Sessions {
		if s, ok := rows[r.SessionID]; ok {
			s.Activity = r.Subagents + r.Background
			rows[r.SessionID] = s
		}
	}
	out := make([]hub.Session, 0, len(rows))
	for _, s := range rows {
		if s.Running {
			s.Needs = b.needs(s.ID)
		}
		out = append(out, s)
	}
	b.listMu.Lock()
	b.listed, b.listedAt = slices.Clone(out), time.Now()
	b.listMu.Unlock()
	return out, nil
}

// needs is what the hub's subscription to a session says it waits for:
// an approval before a question, "" for neither or no subscription.
func (b *backend) needs(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ls := b.sessions[id]
	if ls == nil {
		return ""
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.retired {
		return ""
	}
	out := ""
	for _, p := range ls.prompts {
		switch p.Kind {
		case gateway.PromptApproval:
			return hub.NeedsApproval
		case gateway.PromptClarify:
			out = hub.NeedsQuestion
		}
	}
	return out
}

func (b *backend) create(ctx context.Context, cwd, rowCWD string) (hub.Session, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return hub.Session{}, err
	}
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	var snap gateway.Snapshot
	if err := c.Call(cctx, gateway.MethodSessionCreate, gateway.CreateParams{
		RequestID: uuid.NewString(), Source: gateway.SourceGUI, CWD: cwd, RowCWD: rowCWD,
	}, &snap); err != nil {
		return hub.Session{}, err
	}
	ls := newLive(b, snap.SessionID)
	ls.opMu.Lock()
	ls.pending = true
	b.mu.Lock()
	b.sessions[snap.SessionID] = ls
	b.mu.Unlock()
	b.subscribed(cctx, c, ls, snap)
	ls.setClient(c)
	ls.opMu.Unlock()
	ls.idle()
	title := ""
	if snap.Info.Title != nil {
		title = *snap.Info.Title
	}
	return hub.Session{ID: snap.SessionID, BackendID: b.cfg.ID, Title: title, Live: true, UpdatedAt: time.Now(),
		Source: gateway.SourceGUI}, nil
}

// known pins and returns the hub's subscription to a session when it has
// a snapshot to show, so a viewer can attach while the backend is away.
// It returns nil otherwise.
func (b *backend) known(id string) *live {
	b.mu.Lock()
	defer b.mu.Unlock()
	ls := b.sessions[id]
	if ls == nil {
		return nil
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.retired || ls.snapshot.Seq == 0 {
		return nil
	}
	ls.pins++
	return ls
}

// attachRestartWait is how long opening a session the hub has not seen yet
// keeps retrying while its backend restarts, so a chat picked during a
// gateway handoff opens instead of failing.
const attachRestartWait = 90 * time.Second

// attachable is the session to attach a viewer to: subscribed, or, while the
// backend is away, the copy the hub already has (offline is true then). A
// session the hub has not seen is retried for up to attachRestartWait.
func (b *backend) attachable(ctx context.Context, id string) (*live, bool, error) {
	delay := 250 * time.Millisecond
	deadline := time.Now().Add(attachRestartWait)
	for {
		ls, err := b.session(ctx, id)
		if err == nil {
			return ls, false, nil
		}
		if !gateway.Retryable(err) {
			return nil, false, err
		}
		if known := b.known(id); known != nil {
			return known, true, nil
		}
		if !time.Now().Before(deadline) {
			return nil, false, err
		}
		select {
		case <-ctx.Done():
			return nil, false, err
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
}

// session returns the hub's subscription to a session, subscribing first.
func (b *backend) session(ctx context.Context, id string) (*live, error) {
	for {
		ls, err := b.sessionOnce(ctx, id)
		if !errors.Is(err, errRetired) {
			return ls, err
		}
	}
}

// sessionOnce subscribes and pins the session, so an idle detach cannot
// retire it while a caller uses it; callers unpin. It returns
// errRetired when an idle detach retired the session first.
func (b *backend) sessionOnce(ctx context.Context, id string) (*live, error) {
	b.mu.Lock()
	ls := b.sessions[id]
	if ls == nil {
		ls = newLive(b, id)
		b.sessions[id] = ls
	}
	b.mu.Unlock()
	if _, err := b.ensure(ctx, ls); err != nil {
		ls.opMu.Lock()
		b.mu.Lock()
		ls.mu.Lock()
		if ls.client == nil && len(ls.viewers) == 0 && b.sessions[id] == ls {
			delete(b.sessions, id)
		}
		ls.mu.Unlock()
		b.mu.Unlock()
		ls.opMu.Unlock()
		return nil, err
	}
	b.mu.Lock()
	ls.mu.Lock()
	current := b.sessions[id] == ls && !ls.retired
	if current {
		ls.pins++
	}
	ls.mu.Unlock()
	b.mu.Unlock()
	if !current {
		return nil, errRetired
	}
	return ls, nil
}

// ensure makes ls subscribed on the current connection: a first resume, or
// after a lost connection a replay through session.events.since and then a
// resume.
func (b *backend) ensure(ctx context.Context, ls *live) (*gateway.Client, error) {
	ls.opMu.Lock()
	defer ls.opMu.Unlock()
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	ls.mu.Lock()
	stale := ls.stale
	if ls.client == c && !stale {
		ls.mu.Unlock()
		return c, nil
	}
	ls.pending, ls.early, ls.stale = true, nil, false
	recovering := ls.client != nil && !stale
	epoch, last, oldSub := ls.epoch, ls.gwSeq, ls.subID
	ls.mu.Unlock()
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	if stale && ls.client == c && oldSub != "" {
		_ = c.Call(cctx, gateway.MethodSessionDetach, gateway.DetachParams{SessionID: ls.id, SubscriptionID: oldSub}, nil)
	}
	if recovering {
		var r gateway.EventsSinceResult
		err := c.Call(cctx, gateway.MethodEventsSince, gateway.EventsSinceParams{SessionID: ls.id, LastSequence: last, ReplayEpoch: epoch}, &r)
		if err == nil && !r.SnapshotRequired {
			ls.mu.Lock()
			for _, ev := range r.Events {
				ls.apply(ev)
			}
			ls.mu.Unlock()
			var snap gateway.Snapshot
			if err := c.Call(cctx, gateway.MethodSessionResume, gateway.ResumeParams{SessionID: ls.id}, &snap); err != nil {
				ls.abort()
				return nil, err
			}
			b.reasoning(cctx, c, ls.id)
			ls.mu.Lock()
			ls.subID = snap.SubscriptionID
			ls.generation = max(ls.generation, snap.ExecutionGeneration)
			ls.revision = max(ls.revision, snap.Revision)
			ls.setPrompts(snap.Prompts, true)
			ls.setInputs(snap.Pending)
			ls.flushEarly()
			ls.mu.Unlock()
			ls.setClient(c)
			ls.status(true)
			return c, nil
		}
		if err != nil {
			b.log.Warn("events.since failed, taking a new snapshot", "session", ls.id, "error", err.Error())
		}
	}
	var snap gateway.Snapshot
	if err := c.Call(cctx, gateway.MethodSessionResume, gateway.ResumeParams{SessionID: ls.id}, &snap); err != nil {
		ls.abort()
		return nil, err
	}
	if snap.SessionID != "" && snap.SessionID != ls.id {
		b.mu.Lock()
		b.sessions[snap.SessionID] = ls
		b.mu.Unlock()
	}
	b.subscribed(cctx, c, ls, snap)
	ls.setClient(c)
	if recovering {
		ls.status(true)
	}
	return c, nil
}

func (b *backend) reasoning(ctx context.Context, c *gateway.Client, id string) {
	if b.cfg.ShowReasoning != nil && !*b.cfg.ShowReasoning {
		return
	}
	err := c.Call(ctx, gateway.MethodConfigSet, gateway.ConfigSetParams{SessionID: id, Key: "show_reasoning", Value: "on"}, nil)
	if err != nil {
		b.log.Warn("turn on reasoning", "session", id, "error", err.Error())
	}
}

// subscribed installs a fresh snapshot. ls.pending must be set.
func (b *backend) subscribed(ctx context.Context, c *gateway.Client, ls *live, snap gateway.Snapshot) {
	b.reasoning(ctx, c, snap.SessionID)
	var tail, before []gateway.Event
	if snap.Running {
		var r gateway.EventsSinceResult
		err := c.Call(ctx, gateway.MethodEventsSince, gateway.EventsSinceParams{SessionID: snap.SessionID, ReplayEpoch: snap.ReplayEpoch}, &r)
		if err == nil && !r.SnapshotRequired {
			before, tail = currentTurn(r.Events, snap.LastSequence)
		}
	}
	title := ""
	if snap.Info.Title != nil {
		title = *snap.Info.Title
	}
	prompts := make([]json.RawMessage, 0, len(snap.Prompts))
	for _, p := range snap.Prompts {
		raw, err := json.Marshal(p)
		if err == nil {
			prompts = append(prompts, raw)
		}
	}
	msgs := viewRows(snap.Messages)
	pending := make([]hub.Pending, 0, len(snap.Pending))
	turn := runningAdmission(snap.Pending, "")
	for _, p := range snap.Pending {
		switch {
		case steering(p, turn):
			pending = append(pending, hub.Pending{ID: p.AdmissionID, Status: hub.PendingSteering, Text: p.Text, SubmissionID: sub(p)})
		case p.Status == gateway.AdmissionQueued:
			pending = append(pending, hub.Pending{ID: p.AdmissionID, Status: hub.PendingQueued, Text: p.Text, SubmissionID: sub(p)})
		case p.Status == gateway.AdmissionUnknown:
			pending = append(pending, hub.Pending{ID: p.AdmissionID, Status: hub.PendingLost, Text: p.Text, SubmissionID: sub(p)})
		}
	}
	data, err := json.Marshal(hub.Snapshot{SessionID: snap.SessionID, BackendID: b.cfg.ID, Title: title,
		Running: snap.Running, Messages: msgs, Prompts: prompts, Pending: pending, Info: info(snap.Info),
		ToolBackground: b.capable(ctx, c, gateway.CapToolBackground)})
	if err != nil {
		b.log.Warn("encode snapshot", "session", snap.SessionID, "error", err.Error())
		data = json.RawMessage(`{}`)
	}
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.title = title
	if snap.Running && ls.turnStart.IsZero() {
		ls.turnStart = time.Now()
	}
	ls.subID, ls.epoch, ls.gwSeq = snap.SubscriptionID, snap.ReplayEpoch, snap.LastSequence
	ls.generation, ls.revision = snap.ExecutionGeneration, snap.Revision
	if snap.Info.Vision != nil {
		ls.vision = snap.Info.Vision
	}
	if snap.Info.Provider != nil && *snap.Info.Provider != "" {
		ls.provider = *snap.Info.Provider
	}
	ls.setPrompts(snap.Prompts, false)
	ls.setInputs(snap.Pending)
	ls.turnAdmission = ""
	if snap.Running {
		ls.turnAdmission = runningAdmission(snap.Pending, "")
	}
	ls.subscribedUsage(snap.Running, snap.Info.Usage, before)
	ls.ring, ls.truncated = nil, false
	ls.seq++
	ls.snapshot = hub.Event{Seq: ls.seq, Kind: hub.KindSnapshot, Data: data}
	ls.broadcast(ls.snapshot)
	ls.replayTurn(tail, msgs)
	b.heldEventsLocked(ls, ls.id, snap.SessionID)
	ls.flushEarly()
}

func info(i gateway.SnapshotInfo) hub.Info {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	out := hub.Info{Model: str(i.Model), Provider: str(i.Provider), CWD: str(i.CWD),
		ReasoningEffort: str(i.ReasoningEffort), Usage: i.Usage}
	if i.ContextLength != nil {
		out.ContextLength = *i.ContextLength
	}
	return out
}

// currentTurn splits the frames up to the snapshot's sequence at the
// last message.complete: the frames before the running turn, up to that
// turn end, and the frames of the running turn.
func currentTurn(evs []gateway.Event, last int64) (before, turn []gateway.Event) {
	start := 0
	for i, ev := range evs {
		if ev.Type == hub.KindMessageComplete && ev.Seq <= last {
			start = i + 1
		}
	}
	for _, ev := range evs[start:] {
		if ev.Seq <= last {
			turn = append(turn, ev)
		}
	}
	return evs[:start], turn
}

func (b *backend) call(ctx context.Context, ls *live, method string, params, out any) error {
	return b.callWithin(ctx, ls, 0, method, params, out)
}

// callWithin is call with its own bound: d, or CallTimeout for 0.
func (b *backend) callWithin(ctx context.Context, ls *live, d time.Duration, method string, params, out any) error {
	c, err := b.ensure(ctx, ls)
	if err != nil {
		return err
	}
	if d == 0 {
		d = b.svc.opts.CallTimeout
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
	defer cancel()
	return c.Call(cctx, method, params, out)
}

// readRestartWait bounds how long a read-only call is tried again while
// its backend restarts. A Hermes gateway that stops for a rollout closes the
// hub's socket once and is replaced within seconds.
const readRestartWait = 30 * time.Second

// read is call for a method that changes nothing: while the backend is
// restarting (gateway.Retryable: a closed socket, a refusal while it drains)
// it is sent again on the next connection, with backoff, within ctx and
// readRestartWait, so a page that polls it sees a slower answer instead of
// an error.
func (b *backend) read(ctx context.Context, ls *live, method string, params, out any) error {
	deadline := time.Now().Add(readRestartWait)
	delay := 250 * time.Millisecond
	for {
		err := b.call(ctx, ls, method, params, out)
		if err == nil || !gateway.Retryable(err) || ctx.Err() != nil || !time.Now().Add(delay).Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
}

// detachIdle runs from a timer, with no request behind it.
//
//nolint:contextcheck // see above
func (b *backend) detachIdle(ls *live) {
	ls.opMu.Lock()
	defer ls.opMu.Unlock()
	b.mu.Lock()
	ls.mu.Lock()
	if len(ls.viewers) > 0 || ls.pending || ls.pins > 0 || ls.watched {
		ls.mu.Unlock()
		b.mu.Unlock()
		return
	}
	for id, other := range b.sessions {
		if other == ls {
			delete(b.sessions, id)
		}
	}
	c, sub := ls.client, ls.subID
	ls.client, ls.cur, ls.retired = nil, nil, true
	ls.mu.Unlock()
	b.mu.Unlock()
	if !alive(c) || sub == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.svc.opts.CallTimeout)
	defer cancel()
	if err := c.Call(ctx, gateway.MethodSessionDetach, gateway.DetachParams{SessionID: ls.id, SubscriptionID: sub}, nil); err != nil {
		b.log.Debug("detach", "session", ls.id, "error", err.Error())
	}
}

// Watch keeps each running session of every backend subscribed while it
// runs, checking every interval, so the hub sees the approvals and
// questions of chats nobody has open, such as scheduled jobs. Run it on
// one replica. It returns when ctx ends.
func (s *Service) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, id := range s.order {
			s.backends[id].watchRunning(ctx)
		}
		select {
		case <-ctx.Done():
			for _, b := range s.backends {
				b.unwatch(func(string) bool { return true })
			}
			return
		case <-t.C:
		}
	}
}

func (b *backend) watchRunning(ctx context.Context) {
	c, err := b.conn(ctx)
	if err != nil {
		return
	}
	cctx, cancel := b.timeout(ctx)
	var res gateway.ListResult
	err = c.Call(cctx, gateway.MethodSessionList, gateway.ListParams{Scope: gateway.ScopeLive, Limit: 200}, &res)
	cancel()
	if err != nil {
		b.log.Debug("watch: list sessions", "error", err.Error())
		return
	}
	running := map[string]bool{}
	for _, r := range res.Sessions {
		if !r.Running || ctx.Err() != nil {
			continue
		}
		running[r.SessionID] = true
		ls, err := b.session(ctx, r.SessionID)
		if err != nil {
			b.log.Debug("watch: subscribe", "session", r.SessionID, "error", err.Error())
			continue
		}
		ls.mu.Lock()
		ls.watched = true
		ls.mu.Unlock()
		ls.unpin()
	}
	b.unwatch(func(id string) bool { return !running[id] })
}

// unwatch lets the watched sessions that done picks detach when idle.
func (b *backend) unwatch(done func(id string) bool) {
	b.mu.Lock()
	var idle []*live
	for _, ls := range b.sessions {
		ls.mu.Lock()
		if ls.watched && done(ls.id) {
			ls.watched = false
			idle = append(idle, ls)
		}
		ls.mu.Unlock()
	}
	b.mu.Unlock()
	for _, ls := range idle {
		ls.idle()
	}
}
