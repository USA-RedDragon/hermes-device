package service

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// live is the hub's subscription to one gateway session, shared by every
// viewer of it.
type live struct {
	b  *backend
	id string

	// opMu serializes subscribe, recover and detach.
	opMu   sync.Mutex
	client *gateway.Client

	mu      sync.Mutex
	stale   bool
	retired bool
	// gone is closed when a retired session has left the map, after the
	// gateway was told it is detached; nil when it was not retired by an idle detach.
	gone       chan struct{}
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
	texts map[string]string
	// subs remembers each pending input's submission ID the same way, so
	// the end of a steer can name it after the pending list dropped it.
	subs map[string]string
	// turnAdmission is the admission of the running turn, "" when not
	// known; outcomes is what became of the admissions StopAndSend waits
	// for: "started", or the outcome their message.complete reported.
	turnAdmission string
	// steered holds the admissions the hub handed to the running turn
	// with prompt.steer_queued. A prompt sent again under the same ID
	// comes back as started, and this tells its steer from a turn of its
	// own. It is cleared when the turn ends: Hermes ends a steer with it.
	steered  map[string]bool
	outcomes map[string]string
	// late is set by the snapshot of a running turn whose events Hermes's
	// replay ring no longer had in full (it keeps 512 events, a long reply
	// streams thousands): the reply streaming now has lost its start, so
	// its deltas are held back until it ends (see holdLate). lateHeld says
	// a delta was held.
	late, lateHeld bool
	seq            int64
	snapshot       hub.Event
	ring           []hub.Event
	truncated      bool
	viewers        map[*viewer]struct{}
	idleTimer      *time.Timer
	// vision says whether the session's tools can read images, from the
	// latest info that listed them; nil when none did.
	vision *bool
	// title is the session's title, for notices.
	title string
	// provider is the provider the session last used, "" when unknown.
	provider string
	// turnStart is when the running turn started, or when the hub saw it
	// running first; zero while idle.
	turnStart time.Time
	// watched keeps the session subscribed with no viewer while it runs,
	// so the hub sees its prompts (see Service.Watch).
	watched bool
	// usage is the session's latest token counters, turnBase those at the
	// start of the running turn and turn its tokens so far; nil when not
	// known.
	usage    *counts
	turnBase *counts
	turn     *hub.TurnTokens
	// running are the tool calls that started and did not end, subagents
	// the subagents that run, and gates the approvals whose outcome the
	// hub works out from their tool call, by prompt ID.
	running   map[string]runningTool
	subagents map[string]bool
	gates     map[string]*gate
}

func (ls *live) canSee() *bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.vision
}

type viewer struct {
	ch     chan hub.Event
	closed bool
}

func newLive(b *backend, id string) *live {
	return &live{b: b, id: id, prompts: map[string]gateway.Prompt{}, viewers: map[*viewer]struct{}{},
		seq: time.Now().UnixMicro(), running: map[string]runningTool{}, subagents: map[string]bool{}, gates: map[string]*gate{}}
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
	if ls.subs == nil {
		ls.subs = map[string]string{}
	}
	for _, p := range ps {
		ls.inputs[p.AdmissionID] = p
		ls.texts[p.AdmissionID] = p.Text
		if p.InputID != "" && gateway.Producer(p.InputID) == "" {
			ls.subs[p.AdmissionID] = p.InputID
		}
	}
}

// withText adds what the hub knows about the started input to a
// message.start payload: its text when the payload lacks one, "note" with
// its producer when Hermes queued the input itself (a cron result), so
// viewers do not show it as the user's message, and else the input's
// submission ID, so viewers can tell where the user sent it. ls.mu is held.
func (ls *live) withText(ev gateway.Event) json.RawMessage {
	return ls.startPayload(ev, true)
}

// startPayload is withText; withPrompt false leaves out the text of a
// user's input. ls.mu is held.
func (ls *live) startPayload(ev gateway.Event, withPrompt bool) json.RawMessage {
	if ev.AdmissionID == "" {
		return ev.Payload
	}
	text, ok := ls.texts[ev.AdmissionID]
	in, known := ls.inputs[ev.AdmissionID]
	if !ok && !known {
		return ev.Payload
	}
	delete(ls.texts, ev.AdmissionID)
	note := gateway.Producer(in.InputID)
	var p map[string]any
	if json.Unmarshal(ev.Payload, &p) != nil || p == nil {
		p = map[string]any{}
	}
	switch s, _ := p["text"].(string); {
	case note != "":
		if s == "" {
			p["text"] = text
		}
		p["note"] = note
	case s == "" && withPrompt && text != "":
		p["text"] = text
	}
	if note == "" && in.InputID != "" {
		p[hub.StartSubmissionID] = in.InputID
	}
	if note == "" {
		p[hub.StartAdmissionID] = ev.AdmissionID
	}
	out, err := json.Marshal(p)
	if err != nil {
		return ev.Payload
	}
	return out
}

// stampStart adds when the hub saw a turn start to its message.start
// payload, so a viewer that attaches later shows how long it runs.
func stampStart(payload json.RawMessage, at time.Time) json.RawMessage {
	var p map[string]any
	if json.Unmarshal(payload, &p) != nil || p == nil {
		p = map[string]any{}
	}
	p[hub.StartedAt] = at.UnixMilli()
	out, err := json.Marshal(p)
	if err != nil {
		return payload
	}
	return out
}

// replayTurn emits the events of the turn that ran when the hub
// subscribed, with what the turn's session.info events say about its
// input on its message.start. The prompt's text goes on it only when the
// snapshot's transcript does not show it already. ls.mu is held.
func (ls *live) replayTurn(tail []gateway.Event, msgs []json.RawMessage) {
	_, shown := hub.OpenTurnPrompt(msgs)
	inputs, texts := ls.inputs, ls.texts
	ls.inputs, ls.texts = map[string]gateway.PendingAdmission{}, map[string]string{}
	for _, ev := range tail {
		if ev.Type != gateway.InfoEvent {
			continue
		}
		var p gateway.InfoPayload
		if json.Unmarshal(ev.Payload, &p) == nil {
			for _, in := range p.Pending {
				ls.inputs[in.AdmissionID] = in
				ls.texts[in.AdmissionID] = in.Text
			}
		}
	}
	for _, ev := range tail {
		payload := ev.Payload
		if ev.Type == hub.KindMessageStart {
			ev.Payload = ls.startPayload(ev, !shown)
			if gateway.Producer(ls.inputs[ev.AdmissionID].InputID) != "" {
				ev.Payload = payload
			}
		}
		ls.emitAt(ev.Type, ls.trackTurn(ev), 0)
	}
	ls.inputs, ls.texts = inputs, texts
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

// setPrompts replaces the pending prompts and announces each, so a prompt
// raised before the hub subscribed reaches its users too. A prompt the hub
// held that Hermes no longer lists is over without an answer (a restart of
// Hermes drops the threads that waited on it): it is settled as lost, so
// no card keeps buttons that cannot work. announce also sends a prompt the
// hub did not hold as an event, for viewers that stay attached through a
// resume and so get no snapshot. ls.mu is held.
func (ls *live) setPrompts(ps []gateway.Prompt, announce bool) {
	old := ls.prompts
	ls.prompts = map[string]gateway.Prompt{}
	for _, p := range ps {
		if p.Kind == gateway.PromptApproval || p.Kind == gateway.PromptClarify {
			ls.prompts[p.PromptID] = p
			ls.promptNotice(p)
			if _, known := old[p.PromptID]; announce && !known {
				ls.announcePrompt(p)
			}
		}
	}
	var lost []string
	for id := range old {
		if _, ok := ls.prompts[id]; !ok {
			lost = append(lost, id)
		}
	}
	if len(lost) > 0 {
		slices.Sort(lost)
		ls.lostPrompts(lost)
	}
}

// announcePrompt sends a prompt as the request event Hermes sends for it.
// ls.mu is held.
func (ls *live) announcePrompt(p gateway.Prompt) {
	kind := hub.KindApprovalRequest
	if p.Kind == gateway.PromptClarify {
		kind = hub.KindClarifyRequest
	}
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	ls.emit(kind, data)
}

// lostPrompts settles prompts Hermes no longer lists and nobody answered.
// ls.mu is held.
func (ls *live) lostPrompts(ids []string) {
	data, err := json.Marshal(struct {
		PromptIDs []string `json:"prompt_ids"`
	}{ids})
	if err == nil {
		ls.emit(hub.KindPromptLost, data)
	}
	ls.settle(true, ids...)
}

func promptInfo(p gateway.Prompt) hub.PromptInfo {
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return hub.PromptInfo{ID: p.PromptID, Kind: p.Kind, Choices: append([]string(nil), p.Choices...), Multi: p.MultiSelect,
		Command: str(p.Command), Description: str(p.Description), Subagent: str(p.Subagent), Question: str(p.Question), Timeout: p.Timeout}
}

// promptNotice announces a pending prompt. ls.mu is held.
func (ls *live) promptNotice(p gateway.Prompt) {
	kind := hub.NoticeApproval
	if p.Kind == gateway.PromptClarify {
		kind = hub.NoticeQuestion
	}
	info := promptInfo(p)
	ls.b.svc.notice(hub.Notice{Kind: kind, BackendID: ls.b.cfg.ID, SessionID: ls.id, Key: "prompt:" + ls.id + ":" + p.PromptID,
		Prompt: &info, Title: ls.title})
}

// settled forgets prompts that got an answer. ls.mu is held.
func (ls *live) settled(ids ...string) { ls.settle(false, ids...) }

// settle forgets prompts that got an answer or were cancelled. ls.mu is
// held.
func (ls *live) settle(cancelled bool, ids ...string) {
	var gone []string
	for _, id := range ids {
		if id != "" {
			p, ok := ls.prompts[id]
			delete(ls.prompts, id)
			gone = append(gone, id)
			if ok && p.Kind == gateway.PromptApproval {
				ls.settleOutcome(id, cancelled)
			}
		}
	}
	if len(gone) > 0 {
		ls.b.svc.notice(hub.Notice{Kind: hub.NoticeSettled, BackendID: ls.b.cfg.ID, SessionID: ls.id, PromptIDs: gone,
			Cancelled: cancelled})
	}
}

// longTurn is how long a turn runs before its end is worth a notice.
const longTurn = 2 * time.Minute

// turnNotices tracks turns for notices: a long turn that ends, a turn that
// fails, and a tool an unattended run could not use. ls.mu is held.
func (ls *live) turnNotices(ev gateway.Event) {
	switch ev.Type {
	case hub.KindMessageStart:
		if ls.turnStart.IsZero() {
			ls.turnStart = time.Now()
		}
	case hub.KindMessageComplete:
		var p struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		key := ls.id + ":" + ls.epoch + ":" + strconv.FormatInt(ev.Seq, 10)
		n := hub.Notice{BackendID: ls.b.cfg.ID, SessionID: ls.id, Title: ls.title}
		switch {
		case p.Status == "error":
			n.Kind, n.Key, n.Detail = hub.NoticeFailure, "failed:"+key, "The turn ended with an error."
			ls.b.svc.notice(n)
		case p.Status != "interrupted" && !ls.turnStart.IsZero() && time.Since(ls.turnStart) >= longTurn:
			n.Kind, n.Key = hub.NoticeFinished, "finished:"+key
			ls.b.svc.notice(n)
		}
		ls.turnStart = time.Time{}
	case hub.KindToolComplete:
		var p struct {
			Name       string `json:"name"`
			ResultText string `json:"result_text"`
			Summary    string `json:"summary"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Name == "" {
			return
		}
		text := p.ResultText + "\n" + p.Summary
		if !strings.Contains(text, p.Name+" was not run: ") || !strings.Contains(text, noUserMarker) {
			return
		}
		ls.b.svc.notice(hub.Notice{Kind: hub.NoticeFailure, BackendID: ls.b.cfg.ID, SessionID: ls.id, Title: ls.title,
			Key:    "blocked:" + ls.id + ":" + ls.epoch + ":" + strconv.FormatInt(ev.Seq, 10),
			Detail: "An unattended run could not use " + p.Name + ": it needs your approval, given once with Always."})
	}
}

// noUserMarker is in the message talaria's browser tools give when
// they refuse a step that needs the user in a run with no user present
// (plugin/device/browser.py).
const noUserMarker = "no user is present in this run"

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
	ls.b.obs.observe(ls.id, ev.Type, ev.Payload, time.Now())
	switch ev.Type {
	case gateway.InfoEvent:
		if v := gateway.Vision(ev.Payload); v != nil {
			ls.vision = v
		}
		var p gateway.InfoPayload
		if json.Unmarshal(ev.Payload, &p) == nil {
			if p.Pending != nil {
				ls.setInputs(p.Pending)
			}
			ls.fences(p.Revision, nil)
		}
	case gateway.UpdatedEvent:
		var p struct {
			gateway.MutateResult
			Title *string `json:"title"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			ls.fences(p.Revision, p.ExecutionGeneration)
			if p.Title != nil {
				ls.title = *p.Title
			}
		}
	case hub.KindMessageStart:
		ev.Payload = stampStart(ls.withText(ev), time.Now())
	case hub.KindThinkingDelta, "subagent.thinking":
		ev.Payload = plainThinking(ev.Payload)
	case hub.KindApprovalRequest, hub.KindClarifyRequest:
		var p gateway.Prompt
		if json.Unmarshal(ev.Payload, &p) == nil && p.PromptID != "" {
			switch {
			case ev.Type == hub.KindClarifyRequest:
				p.Kind = gateway.PromptClarify
			case p.Kind == "":
				p.Kind = gateway.PromptApproval
			}
			ls.prompts[p.PromptID] = p
			ls.promptNotice(p)
			ls.gateApproval(p)
		}
	case "approval.settled", "clarify.settled":
		var p struct {
			PromptID string `json:"prompt_id"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			ls.settled(p.PromptID)
		}
	case "approval.cancelled":
		var p struct {
			RequestIDs []string `json:"request_ids"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			ls.settle(true, p.RequestIDs...)
		}
	}
	ev.Payload = ls.trackSettled(ev)
	ev.Payload = ls.trackTurn(ev)
	ls.trackProvider(ev)
	ls.turnNotices(ev)
	if text, ok := failedTurnText(ev); ok {
		ls.turnFailed(text)
	}
	if !ls.holdLate(&ev) {
		ls.emit(ev.Type, ev.Payload)
	}
	ls.trackOutcomes(ev)
}

// holdLate says whether ev is held back from viewers because the turn was
// joined late (see live.late): the deltas of the reply that was streaming
// when the hub subscribed are the rest of a reply without its start, and
// shown as a reply they cut a fenced block in two. Its whole text comes
// with the message.interim or message.complete that ends it; an interim
// then reads as not streamed, so viewers show its text. The first event
// that ends the reply, or that starts something else before any delta,
// ends the hold. ls.mu is held.
func (ls *live) holdLate(ev *gateway.Event) bool {
	if !ls.late {
		return false
	}
	switch ev.Type {
	case hub.KindMessageDelta:
		ls.lateHeld = true
		return true
	case hub.KindMessageInterim:
		ls.late = false
		if ls.lateHeld {
			ev.Payload = notStreamed(ev.Payload)
		}
	case hub.KindMessageStart, hub.KindMessageComplete, hub.KindToolStart, "tool.generating":
		ls.late = false
	case hub.KindReasoningDelta, hub.KindToolComplete:
		// Before any delta these start a new reply.
		ls.late = ls.lateHeld
	}
	return false
}

// notStreamed sets an interim's already_streamed to false.
func notStreamed(payload json.RawMessage) json.RawMessage {
	var p map[string]json.RawMessage
	if json.Unmarshal(payload, &p) != nil || p == nil {
		return payload
	}
	p["already_streamed"] = json.RawMessage("false")
	out, err := json.Marshal(p)
	if err != nil {
		return payload
	}
	return out
}

// emit records an event and sends it to viewers. ls.mu is held.
func (ls *live) emit(kind string, data json.RawMessage) {
	ls.emitAt(kind, data, time.Now().UnixMilli())
}

// emitAt is emit for an event that happened at the Unix millisecond at, or
// at 0 when its time is not known. ls.mu is held.
func (ls *live) emitAt(kind string, data json.RawMessage, at int64) {
	ls.seq++
	e := hub.Event{Seq: ls.seq, Kind: kind, Data: data, At: at}
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
// buffer still has all of them, else the snapshot and everything after it,
// or a new snapshot when the buffer holds a finished turn the held one
// lacks.
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
	case sinceSeq == hub.SinceNow && !ls.truncated:
		v.ch <- ls.snapshotWithPrompts()
		ls.send(v, ls.turnStartSeq())
	case covered:
		ls.send(v, sinceSeq)
	case ls.truncated, ls.turnStartSeq() > 0:
		// The held snapshot is older than finished turns in the buffer. A
		// viewer that opens fresh would draw the old transcript and then
		// every event of those turns one by one, with a render and a scroll
		// each: a warm replica sent megabytes and froze the page. A new
		// snapshot has the transcript as it is, and the buffer starts over.
		// Until it comes the viewer gets the held one to show at once: a
		// fresh snapshot waits for Hermes, a second on a big chat.
		if ls.snapshot.Kind == hub.KindSnapshot {
			held := ls.snapshotWithPrompts()
			v.ch <- hub.Event{Kind: hub.KindSnapshot, Data: held.Data, At: held.At, Provisional: true}
		}
		ls.resnapshotLocked()
	default:
		v.ch <- ls.snapshotWithPrompts()
		ls.send(v, 0)
	}
	ls.viewers[v] = struct{}{}
	if ls.idleTimer != nil {
		ls.idleTimer.Stop()
		ls.idleTimer = nil
	}
	return v.ch, func() { ls.detach(v) }
}

// snapshotWithPrompts is the held snapshot with the prompts the hub holds
// now. The snapshot lists the prompts of when it was taken, and the buffer
// replays only the turn that runs, so an approval a subagent asked for in a
// turn that has ended (it waits on the chat, not on a turn) reached a new
// viewer through neither, while the chat list still showed the badge for
// it. ls.mu is held.
func (ls *live) snapshotWithPrompts() hub.Event {
	ev := ls.snapshot
	if ev.Kind != hub.KindSnapshot {
		return ev
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(ev.Data, &m) != nil || m == nil {
		return ev
	}
	ids := make([]string, 0, len(ls.prompts))
	for id := range ls.prompts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	list := make([]gateway.Prompt, 0, len(ids))
	for _, id := range ids {
		list = append(list, ls.prompts[id])
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return ev
	}
	m["prompts"] = raw
	data, err := json.Marshal(m)
	if err != nil {
		return ev
	}
	ev.Data = data
	return ev
}

// turnStartSeq is the seq of the last turn end in the buffer, so the
// events after it are the turn that runs now. ls.mu is held.
func (ls *live) turnStartSeq() int64 {
	for i := len(ls.ring) - 1; i >= 0; i-- {
		if ls.ring[i].Kind == hub.KindMessageComplete {
			return ls.ring[i].Seq
		}
	}
	return 0
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

// plainThinking drops the decorative face Hermes puts in front of its
// thinking status ("(◔_◔) brainstorming...") and capitalizes the rest.
func plainThinking(payload json.RawMessage) json.RawMessage {
	var p map[string]any
	if json.Unmarshal(payload, &p) != nil {
		return payload
	}
	text, ok := p["text"].(string)
	if !ok {
		return payload
	}
	words := strings.Fields(text)
	for len(words) > 0 && !strings.ContainsFunc(words[0], isASCIIAlnum) {
		words = words[1:]
	}
	plain := strings.Join(words, " ")
	if r, n := utf8.DecodeRuneInString(plain); n > 0 {
		plain = string(unicode.ToUpper(r)) + plain[n:]
	}
	p["text"] = plain
	out, err := json.Marshal(p)
	if err != nil {
		return payload
	}
	return out
}

func isASCIIAlnum(r rune) bool {
	return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))
}
