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
	ctx, stop := context.WithCancel(context.Background())
	s := &Service{backends: map[string]*backend{}, dir: dir, opts: opts, log: opts.Logger, origin: uuid.NewString(), stop: stop}
	for _, c := range cfgs {
		b := &backend{svc: s, cfg: c, dialer: opts.Dialer(c), sessions: map[string]*live{},
			log: s.log.With("backend", c.ID), held: map[string][]*heldPrompt{}, remote: map[string][]hub.HeldInput{},
			kick: make(chan struct{}, 1), sending: map[string]*sessionGate{}}
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
				return
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
	return b.create(ctx)
}

// Attach streams a session's events. See hub.Service.
func (s *Service) Attach(ctx context.Context, user, backendID, sessionID string, sinceSeq int64) (<-chan hub.Event, func(), error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, nil, err
	}
	ls, err := b.session(ctx, sessionID)
	offline := false
	if err != nil {
		if ls = b.known(sessionID); ls == nil || !gateway.Retryable(err) {
			return nil, nil, refusal(err)
		}
		offline = true
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
// ones is held too, to keep their order.
func (s *Service) Prompt(ctx context.Context, user, backendID, sessionID, text string) (hub.PromptResult, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.PromptResult{}, err
	}
	id := hub.SubmissionID(ctx)
	if id == "" {
		id = uuid.NewString()
	}
	release := b.gate(sessionID)
	defer release()
	if b.heldCount(sessionID) == 0 {
		err := b.submit(ctx, sessionID, id, text)
		if err == nil {
			return hub.PromptResult{ID: id}, nil
		}
		if !gateway.Retryable(err) {
			return hub.PromptResult{}, refusal(err)
		}
		b.log.Info("backend unavailable, holding a prompt", "session", sessionID, "error", err.Error())
		return b.hold(user, sessionID, id, text, errors.Is(err, gateway.ErrClosed))
	}
	return b.hold(user, sessionID, id, text, false)
}

// submit sends one prompt.submit. Hermes answers a repeated submission ID
// with the admission it already has, so a retry never runs a prompt twice.
func (b *backend) submit(ctx context.Context, sessionID, id, text string) error {
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return err
	}
	defer ls.unpin()
	return b.call(ctx, ls, gateway.MethodPromptSubmit, gateway.SubmitParams{SessionID: ls.id, Text: text, SubmissionID: id}, nil)
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
	p, ok := ls.prompts[approvalID]
	ls.mu.Unlock()
	if !ok {
		return hub.ErrUnknownApproval
	}
	choice := gateway.ChoiceDeny
	if allow {
		choice = gateway.ChoiceOnce
	}
	return b.call(ctx, ls, gateway.MethodApproval, gateway.ApprovalParams{
		SessionID: ls.id, ExecutionGeneration: p.ExecutionGeneration, PromptID: p.PromptID, Choice: choice,
	}, nil)
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
	for _, ls := range sessions {
		if ls.on(c) {
			ls.status(false)
		}
	}
	b.retry()
}

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
	lists := []gateway.ListParams{{Scope: gateway.ScopeStored, Limit: 200},
		{Scope: gateway.ScopeLive, Limit: 200, IncludeHidden: true}, {Scope: gateway.ScopeLive, Limit: 200}}
	for _, lp := range lists {
		var res gateway.ListResult
		if err := c.Call(cctx, gateway.MethodSessionList, lp, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Sessions {
			s := hub.Session{ID: r.SessionID, BackendID: b.cfg.ID, Title: r.Title, Live: lp.Scope == gateway.ScopeLive,
				Running: r.Running, MessageCount: r.MessageCount, Archived: lp.IncludeHidden}
			if r.StartedAt != nil {
				s.UpdatedAt = time.UnixMilli(int64(*r.StartedAt * 1000))
			}
			rows[s.ID] = s
		}
	}
	out := make([]hub.Session, 0, len(rows))
	for _, s := range rows {
		out = append(out, s)
	}
	return out, nil
}

func (b *backend) create(ctx context.Context) (hub.Session, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return hub.Session{}, err
	}
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	var snap gateway.Snapshot
	if err := c.Call(cctx, gateway.MethodSessionCreate, gateway.CreateParams{
		RequestID: uuid.NewString(), Source: "gui", CWD: b.cfg.CWD,
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
	return hub.Session{ID: snap.SessionID, BackendID: b.cfg.ID, Title: title, Live: true, UpdatedAt: time.Now()}, nil
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
			ls.setPrompts(snap.Prompts)
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
	var tail []gateway.Event
	if snap.Running {
		var r gateway.EventsSinceResult
		err := c.Call(ctx, gateway.MethodEventsSince, gateway.EventsSinceParams{SessionID: snap.SessionID, ReplayEpoch: snap.ReplayEpoch}, &r)
		if err == nil && !r.SnapshotRequired {
			tail = currentTurn(r.Events, snap.LastSequence)
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
	msgs := snap.Messages
	if msgs == nil {
		msgs = []json.RawMessage{}
	}
	pending := make([]hub.Pending, 0, len(snap.Pending))
	for _, p := range snap.Pending {
		switch p.Status {
		case gateway.AdmissionQueued:
			pending = append(pending, hub.Pending{ID: p.AdmissionID, Status: hub.PendingQueued, Text: p.Text})
		case gateway.AdmissionUnknown:
			pending = append(pending, hub.Pending{ID: p.AdmissionID, Status: hub.PendingLost, Text: p.Text})
		}
	}
	data, err := json.Marshal(hub.Snapshot{SessionID: snap.SessionID, BackendID: b.cfg.ID, Title: title,
		Running: snap.Running, Messages: msgs, Prompts: prompts, Pending: pending, Info: info(snap.Info)})
	if err != nil {
		b.log.Warn("encode snapshot", "session", snap.SessionID, "error", err.Error())
		data = json.RawMessage(`{}`)
	}
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.subID, ls.epoch, ls.gwSeq = snap.SubscriptionID, snap.ReplayEpoch, snap.LastSequence
	ls.generation, ls.revision = snap.ExecutionGeneration, snap.Revision
	ls.setPrompts(snap.Prompts)
	ls.setInputs(snap.Pending)
	ls.ring, ls.truncated = nil, false
	ls.seq++
	ls.snapshot = hub.Event{Seq: ls.seq, Kind: hub.KindSnapshot, Data: data}
	ls.broadcast(ls.snapshot)
	for _, ev := range tail {
		ls.emit(ev.Type, ev.Payload)
	}
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

// currentTurn is the frames of the running turn: those after the last
// message.complete, up to the snapshot's sequence.
func currentTurn(evs []gateway.Event, last int64) []gateway.Event {
	start := 0
	for i, ev := range evs {
		if ev.Type == hub.KindMessageComplete && ev.Seq <= last {
			start = i + 1
		}
	}
	var out []gateway.Event
	for _, ev := range evs[start:] {
		if ev.Seq <= last {
			out = append(out, ev)
		}
	}
	return out
}

func (b *backend) call(ctx context.Context, ls *live, method string, params, out any) error {
	c, err := b.ensure(ctx, ls)
	if err != nil {
		return err
	}
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	return c.Call(cctx, method, params, out)
}

// detachIdle runs from a timer, with no request behind it.
//
//nolint:contextcheck // see above
func (b *backend) detachIdle(ls *live) {
	ls.opMu.Lock()
	defer ls.opMu.Unlock()
	b.mu.Lock()
	ls.mu.Lock()
	if len(ls.viewers) > 0 || ls.pending || ls.pins > 0 {
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
