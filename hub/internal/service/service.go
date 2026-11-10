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
}

// Service is the hub core.
type Service struct {
	backends map[string]*backend
	order    []string
	dir      Directory
	opts     Options
	log      *slog.Logger
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
	s := &Service{backends: map[string]*backend{}, dir: dir, opts: opts, log: opts.Logger}
	for _, c := range cfgs {
		b := &backend{svc: s, cfg: c, dialer: opts.Dialer(c), sessions: map[string]*live{},
			log: s.log.With("backend", c.ID)}
		s.backends[c.ID] = b
		s.order = append(s.order, c.ID)
	}
	return s
}

// Close drops every gateway connection. Sessions stay owned by the gateways.
func (s *Service) Close() {
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
	if err != nil {
		return nil, nil, err
	}
	ch, cancel := ls.attach(sinceSeq)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ch, cancel, nil
}

// Prompt submits text as the next turn of a session.
func (s *Service) Prompt(ctx context.Context, user, backendID, sessionID, text string) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return err
	}
	return b.call(ctx, ls, gateway.MethodPromptSubmit, gateway.SubmitParams{
		SessionID: ls.id, Text: text, SubmissionID: uuid.NewString(),
	}, nil)
}

// Interrupt stops the session's running turn.
func (s *Service) Interrupt(ctx context.Context, user, backendID, sessionID string) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return err
	}
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
		return err
	}
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
}

func (b *backend) lastError() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
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

var errClosed = errors.New("service closed")

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
	return c, nil
}

func (b *backend) watch(c *gateway.Client) {
	<-c.Done()
	b.mu.Lock()
	if b.client == c {
		b.client = nil
	}
	if c.Err() != nil && !b.closed {
		b.lastErr = c.Err().Error()
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
		ls.status(false)
	}
	b.retry()
}

// retry reconnects and re-subscribes every session that has a viewer,
// with backoff, until that works or no viewer is left.
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
				if _, err := b.ensure(context.Background(), ls); err != nil {
					b.log.Warn("re-subscribe", "session", ls.id, "error", err.Error())
					failed = true
					break
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
	for _, scope := range []string{"stored", "live"} {
		var res gateway.ListResult
		if err := c.Call(cctx, gateway.MethodSessionList, gateway.ListParams{Scope: scope, Limit: 200}, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Sessions {
			s := hub.Session{ID: r.SessionID, BackendID: b.cfg.ID, Title: r.Title, Live: scope == "live"}
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
	ls.client = c
	ls.opMu.Unlock()
	ls.idle()
	title := ""
	if snap.Info.Title != nil {
		title = *snap.Info.Title
	}
	return hub.Session{ID: snap.SessionID, BackendID: b.cfg.ID, Title: title, Live: true, UpdatedAt: time.Now()}, nil
}

// session returns the hub's subscription to a session, subscribing first.
func (b *backend) session(ctx context.Context, id string) (*live, error) {
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
	ls.idle()
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
			ls.setPrompts(snap.Prompts)
			ls.flushEarly()
			ls.mu.Unlock()
			ls.client = c
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
	ls.client = c
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
		raw, _ := json.Marshal(p)
		prompts = append(prompts, raw)
	}
	msgs := snap.Messages
	if msgs == nil {
		msgs = []json.RawMessage{}
	}
	data, _ := json.Marshal(hub.Snapshot{SessionID: snap.SessionID, BackendID: b.cfg.ID, Title: title,
		Running: snap.Running, Messages: msgs, Prompts: prompts})
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.subID, ls.epoch, ls.gwSeq = snap.SubscriptionID, snap.ReplayEpoch, snap.LastSequence
	ls.generation = snap.ExecutionGeneration
	ls.setPrompts(snap.Prompts)
	ls.ring, ls.truncated = nil, false
	ls.seq++
	ls.snapshot = hub.Event{Seq: ls.seq, Kind: hub.KindSnapshot, Data: data}
	ls.broadcast(ls.snapshot)
	for _, ev := range tail {
		ls.emit(ev.Type, ev.Payload)
	}
	ls.flushEarly()
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

func (b *backend) detachIdle(ls *live) {
	ls.opMu.Lock()
	defer ls.opMu.Unlock()
	b.mu.Lock()
	ls.mu.Lock()
	if len(ls.viewers) > 0 || ls.pending {
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
	ls.client = nil
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
