package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// maxHeld is how many prompts the hub holds for one session.
const maxHeld = 50

// relayTextLen bounds the prompt text a relayed event carries, so that it
// stays within a PostgreSQL NOTIFY payload even when JSON escapes every
// byte to six.
const relayTextLen = 1000

// Relay carries held-prompt events to the other hub replicas that share a
// store, so a page whose stream runs on another replica than the one that
// holds its prompt still hears what became of it.
type Relay interface {
	Publish(ctx context.Context, msg []byte) error
	Listen(ctx context.Context, fn func([]byte), onErr func(error))
}

// heldPrompt is a prompt waiting for its backend to come back.
type heldPrompt struct {
	id, user, text string
	atts           []gateway.Attachment
	until          time.Time
	// maybe is set once a try lost its connection after the prompt may
	// have gone out, so Hermes may have it although no answer came.
	maybe bool
	// sending is set while the flusher submits it, so drain leaves it to
	// the flusher to report.
	sending bool
}

// errTransient is a check the hub could not make right now, so the held
// prompt waits for the next try.
var errTransient = errors.New("try again later")

// notSentMessage says why a held prompt was not sent, and that Hermes may
// have it after all when a try lost its answer.
func notSentMessage(p *heldPrompt, why string) string {
	if p.maybe {
		return why + " The agent may have received it before the connection dropped: check before you send it again."
	}
	return why
}

func (p *heldPrompt) input() hub.HeldInput {
	return hub.HeldInput{ID: p.id, Text: p.text, HeldUntil: p.until}
}

// relayed is one event between replicas.
type relayed struct {
	Origin    string        `json:"o"`
	BackendID string        `json:"b"`
	SessionID string        `json:"s"`
	Kind      string        `json:"k"`
	Data      hub.HeldInput `json:"d"`
}

// heldCount is how many prompts of a session wait for the backend.
func (b *backend) heldCount(sessionID string) int {
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	return len(b.held[sessionID])
}

// heldEventsLocked re-sends a session's held prompts after a snapshot
// cleared what viewers show. b.holdMu and ls.mu are held.
func (b *backend) heldEventsLocked(ls *live, ids ...string) {
	seen := map[*heldPrompt]bool{}
	for _, id := range ids {
		for _, p := range b.held[id] {
			if seen[p] {
				continue
			}
			seen[p] = true
			if data, err := json.Marshal(p.input()); err == nil {
				ls.emit(hub.KindInputHeld, data)
			}
		}
		now := time.Now()
		for _, in := range b.remote[id] {
			if in.HeldUntil.After(now) {
				if data, err := json.Marshal(in); err == nil {
					ls.emit(hub.KindInputHeld, data)
				}
			}
		}
	}
}

// remoteLocked keeps the prompts other replicas hold, from their relayed
// events, so this replica's viewers see them again after a snapshot.
// b.holdMu is held.
func (b *backend) remoteLocked(sessionID, kind string, in hub.HeldInput) {
	now := time.Now()
	q := slices.DeleteFunc(b.remote[sessionID], func(x hub.HeldInput) bool {
		return x.ID == in.ID || !x.HeldUntil.After(now)
	})
	if kind == hub.KindInputHeld {
		q = append(q, in)
	}
	if len(q) == 0 {
		delete(b.remote, sessionID)
		return
	}
	b.remote[sessionID] = q
}

// hold queues a prompt the backend could not take and starts the flusher.
// maybe marks a prompt whose first try lost its connection after it may
// have gone out.
func (b *backend) hold(user, sessionID, id, text string, atts []gateway.Attachment, maybe bool) (hub.PromptResult, error) {
	until := time.Now().Add(b.svc.opts.HoldTimeout)
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	if b.draining {
		return hub.PromptResult{}, hub.ErrShuttingDown
	}
	for _, p := range b.held[sessionID] {
		if p.id == id {
			return hub.PromptResult{ID: id, Held: true, HeldUntil: p.until}, nil
		}
	}
	if len(b.held[sessionID]) >= maxHeld {
		return hub.PromptResult{}, hub.ErrHoldFull
	}
	p := &heldPrompt{id: id, user: user, text: text, atts: atts, until: until, maybe: maybe}
	b.held[sessionID] = append(b.held[sessionID], p)
	b.eventLocked(sessionID, hub.KindInputHeld, p.input())
	if !b.flushing {
		b.flushing = true
		go b.flush()
	} else {
		b.wake()
	}
	return hub.PromptResult{ID: id, Held: true, HeldUntil: until}, nil
}

// wake makes a waiting flusher try again now.
func (b *backend) wake() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// eventLocked tells this replica's viewers of a session about a held
// prompt, and the other replicas through the relay. b.holdMu is held, so
// events reach viewers in the order the queue changed.
func (b *backend) eventLocked(sessionID, kind string, in hub.HeldInput) {
	b.emitLocal(sessionID, kind, in)
	r := b.svc.relayOut
	if r == nil {
		return
	}
	if len(in.Text) > relayTextLen {
		cut := relayTextLen
		for cut > 0 && !utf8.RuneStart(in.Text[cut]) {
			cut--
		}
		in.Text = in.Text[:cut] + "..."
	}
	msg, err := json.Marshal(relayed{Origin: b.svc.origin, BackendID: b.cfg.ID, SessionID: sessionID, Kind: kind, Data: in})
	if err != nil {
		return
	}
	b.svc.relayPending.Add(1)
	select {
	case r <- msg:
	default:
		b.svc.relayPending.Add(-1)
		b.log.Warn("relay queue full, other replicas miss a held message event", "session", sessionID)
	}
}

func (b *backend) emitLocal(sessionID, kind string, in hub.HeldInput) {
	data, err := json.Marshal(in)
	if err != nil {
		return
	}
	b.mu.Lock()
	ls := b.sessions[sessionID]
	b.mu.Unlock()
	if ls == nil {
		return
	}
	ls.mu.Lock()
	ls.emit(kind, data)
	ls.mu.Unlock()
}

// settle takes a held prompt off the queue and reports it. A prompt that
// is gone already (shutdown gave up on it) is still reported as sent, so
// viewers learn it went through after all.
func (b *backend) settle(sessionID string, p *heldPrompt, kind, reason, message string) {
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	q := b.held[sessionID]
	i := slices.Index(q, p)
	if i < 0 && kind != hub.KindInputSent {
		return
	}
	if i >= 0 {
		q = slices.Delete(q, i, i+1)
		if len(q) == 0 {
			delete(b.held, sessionID)
		} else {
			b.held[sessionID] = q
		}
	}
	if kind == hub.KindInputNotSent {
		message = notSentMessage(p, message)
	}
	in := hub.HeldInput{ID: p.id, Reason: reason, Message: message}
	if kind == hub.KindInputNotSent {
		in.Text = p.text
	}
	b.eventLocked(sessionID, kind, in)
}

// heads returns the oldest held prompt of each session.
func (b *backend) heads() map[string]*heldPrompt {
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	out := make(map[string]*heldPrompt, len(b.held))
	for id, q := range b.held {
		if len(q) > 0 {
			out[id] = q[0]
		}
	}
	return out
}

// expire gives up on the held prompts past their time and returns the
// earliest time left, or zero when nothing is held.
func (b *backend) expire(now time.Time) time.Time {
	var expired []struct {
		sid string
		p   *heldPrompt
	}
	var next time.Time
	b.holdMu.Lock()
	for sid, q := range b.held {
		for _, p := range q {
			if !p.until.After(now) {
				expired = append(expired, struct {
					sid string
					p   *heldPrompt
				}{sid, p})
			} else if next.IsZero() || p.until.Before(next) {
				next = p.until
			}
		}
	}
	b.holdMu.Unlock()
	for _, e := range expired {
		b.log.Warn("held prompt timed out", "session", e.sid)
		b.settle(e.sid, e.p, hub.KindInputNotSent, hub.NotSentTimeout, "The agent did not come back in time.")
	}
	return next
}

// flushStep submits held prompts in order. A session whose try fails
// with a retryable error waits for the next round; the other sessions go
// on. It reports whether any prompt was settled and whether any session
// is still stalled.
//
//nolint:contextcheck // the flusher outlives the request that held the prompt
func (b *backend) flushStep() (progress, stalled bool) {
	skip := map[string]bool{}
	for {
		heads := b.heads()
		ids := make([]string, 0, len(heads))
		for id := range heads {
			if !skip[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return progress, len(skip) > 0
		}
		slices.Sort(ids)
		for _, sid := range ids {
			p := heads[sid]
			if !p.until.After(time.Now()) {
				skip[sid] = true
				continue
			}
			err := b.submitHeld(sid, p)
			switch {
			case err == nil:
				b.log.Info("sent a held prompt", "session", sid)
				b.settle(sid, p, hub.KindInputSent, "", "")
				progress = true
			case b.isDraining():
				b.settle(sid, p, hub.KindInputNotSent, hub.NotSentShutdown, msgShutdown)
				progress = true
			case gateway.Retryable(err), errors.Is(err, errClosed), errors.Is(err, errTransient):
				if errors.Is(err, gateway.ErrClosed) {
					b.holdMu.Lock()
					p.maybe = true
					b.holdMu.Unlock()
				}
				skip[sid] = true
			default:
				b.log.Warn("backend refused a held prompt", "session", sid, "error", err.Error())
				b.settle(sid, p, hub.KindInputNotSent, hub.NotSentRefused, refusedMessage(err))
				progress = true
			}
		}
	}
}

const msgShutdown = "The hub restarted before the agent was back."

func (b *backend) isDraining() bool {
	b.holdMu.Lock()
	defer b.holdMu.Unlock()
	return b.draining
}

func (b *backend) submitHeld(sessionID string, p *heldPrompt) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.svc.opts.CallTimeout)
	defer cancel()
	ok, err := b.svc.allowed(ctx, p.user, b)
	if err != nil {
		return fmt.Errorf("%w: %w", errTransient, err)
	}
	if !ok {
		return hub.ErrForbidden
	}
	b.holdMu.Lock()
	p.sending = true
	b.holdMu.Unlock()
	defer func() {
		b.holdMu.Lock()
		p.sending = false
		b.holdMu.Unlock()
	}()
	_, err = b.submit(ctx, sessionID, p.id, p.text, p.atts)
	return err
}

func refusedMessage(err error) string {
	err = refusal(err)
	switch {
	case errors.Is(err, hub.ErrForbidden):
		return "You can no longer use this backend."
	case errors.Is(err, hub.ErrUnknownSession):
		return "This chat no longer exists."
	}
	return "The agent did not accept it."
}

// flush runs while prompts are held: it retries with backoff, sooner when
// woken, and gives up on prompts whose time is up.
func (b *backend) flush() {
	delay := b.svc.opts.HoldRetryMin
	for {
		if b.isClosed() {
			b.holdMu.Lock()
			b.flushing = false
			b.holdMu.Unlock()
			return
		}
		next := b.expire(time.Now())
		progress, stalled := b.flushStep()
		if !stalled {
			b.holdMu.Lock()
			if len(b.held) == 0 {
				b.flushing = false
				b.holdMu.Unlock()
				return
			}
			b.holdMu.Unlock()
		}
		if progress {
			delay = b.svc.opts.HoldRetryMin
		}
		wait := delay
		if !next.IsZero() {
			wait = min(wait, max(time.Until(next), 0))
		}
		t := time.NewTimer(wait)
		select {
		case <-b.kick:
			delay = b.svc.opts.HoldRetryMin
		case <-t.C:
			delay = min(delay*2, b.svc.opts.HoldRetryMax)
		}
		t.Stop()
	}
}

// drain gives up on every held prompt: the hub is shutting down.
func (b *backend) drain() {
	b.holdMu.Lock()
	b.draining = true
	held := b.held
	b.held = map[string][]*heldPrompt{}
	for sid, q := range held {
		for _, p := range q {
			if p.sending {
				b.held[sid] = append(b.held[sid], p)
				continue
			}
			in := p.input()
			in.HeldUntil, in.Reason, in.Message = time.Time{}, hub.NotSentShutdown, notSentMessage(p, msgShutdown)
			b.eventLocked(sid, hub.KindInputNotSent, in)
		}
	}
	b.holdMu.Unlock()
	b.wake()
}

// relay sends this replica's events to the others and replays theirs to
// this replica's viewers, until ctx ends.
func (s *Service) relay(ctx context.Context, r Relay) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-s.relayOut:
				pctx, cancel := context.WithTimeout(ctx, s.opts.CallTimeout)
				if err := r.Publish(pctx, msg); err != nil && ctx.Err() == nil {
					s.log.Warn("relay a held message event", "error", err.Error())
				}
				cancel()
				s.relayPending.Add(-1)
			}
		}
	}()
	r.Listen(ctx, func(msg []byte) {
		var m relayed
		if json.Unmarshal(msg, &m) != nil || m.Origin == s.origin {
			return
		}
		b := s.backends[m.BackendID]
		if b == nil {
			return
		}
		b.holdMu.Lock()
		b.remoteLocked(m.SessionID, m.Kind, m.Data)
		b.emitLocal(m.SessionID, m.Kind, m.Data)
		b.holdMu.Unlock()
	}, func(err error) {
		s.log.Warn("relay listener reconnecting", "error", err.Error())
	})
}

// Drain gives up on every held prompt and tells its viewers it was not
// sent. Prompts held after it fail with hub.ErrShuttingDown. Call it on shutdown,
// before the event streams end.
func (s *Service) Drain() {
	for _, b := range s.backends {
		b.drain()
	}
}

// relayFlushTimeout bounds how long Close waits for queued relay events.
const relayFlushTimeout = 3 * time.Second

// flushRelay waits until the relay published every queued event, or the
// timeout passed.
func (s *Service) flushRelay() {
	deadline := time.Now().Add(relayFlushTimeout)
	for s.relayPending.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
