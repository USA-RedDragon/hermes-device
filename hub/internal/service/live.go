package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// live is the hub's subscription to one gateway session, shared by every
// viewer of it.
type live struct {
	b  *backend
	id string

	// opMu serializes subscribe, recover and detach.
	opMu   sync.Mutex
	client *gateway.Client

	mu         sync.Mutex
	stale      bool
	retired    bool
	pins       int
	cur        *gateway.Client
	pending    bool
	early      []gateway.Event
	subID      string
	epoch      string
	gwSeq      int64
	generation int64
	// revision is the session's runtime revision, which session.mutate
	// is fenced on. Turns and settings changes move it.
	revision int64
	prompts  map[string]gateway.Prompt
	inputs   map[string]gateway.PendingAdmission
	// texts remembers each pending input's text until its turn starts, so
	// message.start can carry it and a replay shows who said what.
	texts     map[string]string
	seq       int64
	snapshot  hub.Event
	ring      []hub.Event
	truncated bool
	viewers   map[*viewer]struct{}
	idleTimer *time.Timer
}

type viewer struct {
	ch     chan hub.Event
	closed bool
}

func newLive(b *backend, id string) *live {
	return &live{b: b, id: id, prompts: map[string]gateway.Prompt{}, viewers: map[*viewer]struct{}{},
		seq: time.Now().UnixMicro()}
}

// setClient records the connection ls is subscribed on. ls.opMu is held.
func (ls *live) setClient(c *gateway.Client) {
	ls.client = c
	ls.mu.Lock()
	ls.cur = c
	ls.mu.Unlock()
}

func (ls *live) on(c *gateway.Client) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.cur == c
}

func (ls *live) onAlive() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return alive(ls.cur)
}

func (ls *live) unpin() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.pins--
	if ls.pins == 0 && len(ls.viewers) == 0 {
		ls.idleLocked()
	}
}

// fail tells viewers the session cannot be followed any more and drops them.
func (ls *live) fail(err error) {
	data, merr := json.Marshal(map[string]string{"message": err.Error()})
	if merr != nil {
		data = json.RawMessage(`{"message":"session lost"}`)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.seq++
	ls.broadcast(hub.Event{Seq: ls.seq, Kind: hub.KindError, Data: data})
	for v := range ls.viewers {
		v.closed = true
		close(v.ch)
		delete(ls.viewers, v)
	}
	ls.idleLocked()
}

func (ls *live) viewerCount() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.viewers)
}

func (ls *live) abort() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.pending, ls.early = false, nil
}

func (ls *live) handle(ev gateway.Event) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.pending {
		ls.early = append(ls.early, ev)
		return
	}
	ls.apply(ev)
}

// flushEarly applies the frames that arrived while a request was in
// flight. ls.mu is held.
func (ls *live) flushEarly() {
	for _, ev := range ls.early {
		ls.apply(ev)
	}
	ls.pending, ls.early = false, nil
}

func (ls *live) setInputs(ps []gateway.PendingAdmission) {
	ls.inputs = map[string]gateway.PendingAdmission{}
	if ls.texts == nil {
		ls.texts = map[string]string{}
	}
	for _, p := range ps {
		ls.inputs[p.AdmissionID] = p
		ls.texts[p.AdmissionID] = p.Text
	}
}

// withText adds the started input's text to a message.start payload that
// lacks one. ls.mu is held.
func (ls *live) withText(ev gateway.Event) json.RawMessage {
	text, ok := ls.texts[ev.AdmissionID]
	if !ok || ev.AdmissionID == "" {
		return ev.Payload
	}
	delete(ls.texts, ev.AdmissionID)
	var p map[string]any
	if json.Unmarshal(ev.Payload, &p) != nil || p == nil {
		p = map[string]any{}
	}
	if s, _ := p["text"].(string); s != "" {
		return ev.Payload
	}
	p["text"] = text
	out, err := json.Marshal(p)
	if err != nil {
		return ev.Payload
	}
	return out
}

// setFences takes the fences a stale-fence refusal carries as they are.
// ls.mu is held.
func (ls *live) setFences(f gateway.Fences) {
	ls.revision = *f.Revision
	if f.ExecutionGeneration != nil {
		ls.generation = *f.ExecutionGeneration
	}
}

// fences records a newer revision and generation. ls.mu is held.
func (ls *live) fences(revision, generation *int64) {
	if revision != nil && *revision > ls.revision {
		ls.revision = *revision
	}
	if generation != nil && *generation > ls.generation {
		ls.generation = *generation
	}
}

func (ls *live) setPrompts(ps []gateway.Prompt) {
	ls.prompts = map[string]gateway.Prompt{}
	for _, p := range ps {
		if p.Kind == "approval" {
			ls.prompts[p.PromptID] = p
		}
	}
}

// apply passes one gateway frame to viewers once, in order. ls.mu is held.
func (ls *live) apply(ev gateway.Event) {
	if ev.Type == gateway.ReplayGapEvent {
		ls.b.log.Warn("gateway dropped the subscription, taking a new snapshot", "session", ls.id)
		ls.resnapshotLocked()
		return
	}
	if ev.ReplayEpoch != "" && ev.ReplayEpoch != ls.epoch {
		ls.b.log.Info("replay epoch changed", "session", ls.id)
		ls.epoch, ls.gwSeq = ev.ReplayEpoch, 0
	}
	if ev.Seq != 0 {
		if ev.Seq <= ls.gwSeq {
			return
		}
		ls.gwSeq = ev.Seq
	}
	if ev.ExecutionGeneration != nil && *ev.ExecutionGeneration > ls.generation {
		ls.generation = *ev.ExecutionGeneration
	}
	switch ev.Type {
	case gateway.InfoEvent:
		var p gateway.InfoPayload
		if json.Unmarshal(ev.Payload, &p) == nil {
			if p.Pending != nil {
				ls.setInputs(p.Pending)
			}
			ls.fences(p.Revision, nil)
		}
	case gateway.UpdatedEvent:
		var p gateway.MutateResult
		if json.Unmarshal(ev.Payload, &p) == nil {
			ls.fences(p.Revision, p.ExecutionGeneration)
		}
	case hub.KindMessageStart:
		ev.Payload = ls.withText(ev)
	case hub.KindApprovalRequest:
		var p gateway.Prompt
		if json.Unmarshal(ev.Payload, &p) == nil && p.PromptID != "" {
			if p.Kind == "" {
				p.Kind = "approval"
			}
			ls.prompts[p.PromptID] = p
		}
	case "approval.settled":
		var p struct {
			PromptID string `json:"prompt_id"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			delete(ls.prompts, p.PromptID)
		}
	case "approval.cancelled":
		var p struct {
			RequestIDs []string `json:"request_ids"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			for _, id := range p.RequestIDs {
				delete(ls.prompts, id)
			}
		}
	}
	ls.emit(ev.Type, ev.Payload)
}

// emit records an event and sends it to viewers. ls.mu is held.
func (ls *live) emit(kind string, data json.RawMessage) {
	ls.seq++
	e := hub.Event{Seq: ls.seq, Kind: kind, Data: data}
	ls.ring = append(ls.ring, e)
	if n := ls.b.svc.opts.RingSize; len(ls.ring) > n {
		ls.ring = append([]hub.Event(nil), ls.ring[len(ls.ring)-n/2:]...)
		ls.truncated = true
	}
	ls.broadcast(e)
}

// broadcast sends e to every viewer, dropping any that is full. ls.mu is held.
func (ls *live) broadcast(e hub.Event) {
	for v := range ls.viewers {
		select {
		case v.ch <- e:
		default:
			ls.b.log.Warn("viewer too slow, dropping it", "session", ls.id)
			v.closed = true
			close(v.ch)
			delete(ls.viewers, v)
		}
	}
	if len(ls.viewers) == 0 {
		ls.idleLocked()
	}
}

func (ls *live) status(connected bool) {
	data := json.RawMessage(`{"connected":false}`)
	if connected {
		data = json.RawMessage(`{"connected":true}`)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.seq++
	ls.broadcast(hub.Event{Seq: ls.seq, Kind: hub.KindBackendStatus, Data: data})
}

// attach adds a viewer: the buffered events after sinceSeq when the
// buffer still has all of them, else the snapshot and everything after it.
// A fresh snapshot it starts outlives the request.
//
//nolint:contextcheck // see above
func (ls *live) attach(sinceSeq int64) (<-chan hub.Event, func()) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	v := &viewer{ch: make(chan hub.Event, ls.b.svc.opts.ViewerBuffer+len(ls.ring)+1)}
	covered := sinceSeq >= ls.snapshot.Seq && sinceSeq <= ls.seq &&
		(!ls.truncated || len(ls.ring) == 0 || ls.ring[0].Seq <= sinceSeq+1)
	switch {
	case covered:
		ls.send(v, sinceSeq)
	case ls.truncated:
		ls.resnapshotLocked()
	default:
		v.ch <- ls.snapshot
		ls.send(v, 0)
	}
	ls.viewers[v] = struct{}{}
	if ls.idleTimer != nil {
		ls.idleTimer.Stop()
		ls.idleTimer = nil
	}
	return v.ch, func() { ls.detach(v) }
}

// resnapshotLocked starts a fresh subscription; viewers get its snapshot.
// ls.mu is held. The new subscription outlives any request.
//
//nolint:contextcheck // see above
func (ls *live) resnapshotLocked() {
	if ls.stale {
		return
	}
	ls.stale = true
	go func() {
		if _, err := ls.b.ensure(context.Background(), ls); err != nil {
			ls.b.log.Warn("re-snapshot", "session", ls.id, "error", err.Error())
		}
	}()
}

// send queues the buffered events after sinceSeq. ls.mu is held.
func (ls *live) send(v *viewer, sinceSeq int64) {
	for _, e := range ls.ring {
		if e.Seq <= sinceSeq {
			continue
		}
		select {
		case v.ch <- e:
		default:
			return
		}
	}
}

func (ls *live) detach(v *viewer) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !v.closed {
		v.closed = true
		close(v.ch)
		delete(ls.viewers, v)
	}
	if len(ls.viewers) == 0 {
		ls.idleLocked()
	}
}

func (ls *live) idle() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.viewers) == 0 {
		ls.idleLocked()
	}
}

func (ls *live) idleLocked() {
	if ls.idleTimer != nil {
		return
	}
	ls.idleTimer = time.AfterFunc(ls.b.svc.opts.IdleDetach, func() {
		ls.mu.Lock()
		ls.idleTimer = nil
		ls.mu.Unlock()
		ls.b.detachIdle(ls)
	})
}
