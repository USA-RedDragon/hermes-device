package service_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

// pageReply is a reply of about n bytes: prose, a page block, more prose.
func pageReply(n int) string {
	var b strings.Builder
	b.WriteString("Here is the design.\n\n```page\n<!doctype html>\n<html><head><style>\n")
	for i := 0; b.Len() < n-200; i++ {
		b.WriteString(".picker .pbody { padding: .2rem 1rem .8rem; display: flex; flex-direction: column; gap: .75rem; }\n")
	}
	b.WriteString("</style></head><body><h1>Connect a model</h1></body></html>\n```\n\nPick one of the two options.")
	return b.String()
}

// deltas cuts text into stream deltas of size bytes.
func deltas(text string, size int) []string {
	var out []string
	for len(text) > size {
		out = append(out, text[:size])
		text = text[size:]
	}
	return append(out, text)
}

// A chat opened while a long reply streams joins it after Hermes's replay
// ring dropped the reply's start. The hub must not pass on the rest of the
// reply as if it were the whole reply: that showed a page block's second
// half as raw text without its opening fence. The reply's text shows once
// it ends, whole.
func TestLateJoinNeverStreamsAReplyWithoutItsStart(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{ViewerBuffer: 1 << 15})
	e.gw.ReplayMax = 512
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-long", Live: true, Running: true,
		Messages: []map[string]any{{kRole: roleUser, kText: "make the page"}}})
	reply := pageReply(60 << 10)
	parts := deltas(reply, 12)
	cut := len(parts) / 6
	e.gw.Publish("s-long", "message.start", map[string]any{})
	for _, d := range parts[:cut] {
		e.gw.Publish("s-long", "message.delta", map[string]any{kText: d})
	}
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "s-long", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", ev.Kind)
	}
	for _, d := range parts[cut:] {
		e.gw.Publish("s-long", "message.delta", map[string]any{kText: d})
	}
	e.gw.FinishReply("s-long", reply)
	var streamed strings.Builder
	for _, ev := range until(t, ch, hub.KindMessageComplete) {
		if ev.Kind == hub.KindMessageDelta {
			streamed.WriteString(text(t, ev))
		}
		if ev.Kind == hub.KindMessageComplete && text(t, ev) != reply {
			t.Fatalf("message.complete text has %d bytes, want the whole reply (%d)", len(text(t, ev)), len(reply))
		}
	}
	if got := streamed.String(); !strings.HasPrefix(reply, got) {
		t.Fatalf("streamed %d bytes that start mid-reply: %q", len(got), got[:min(len(got), 60)])
	}
}

// A chat that joins a running turn while Hermes still has all of it gets
// the reply from its start, as before.
func TestJoinWithWholeReplayStreamsTheReply(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	e.gw.ReplayMax = 512
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-short", Live: true, Running: true})
	e.gw.Publish("s-short", "message.start", map[string]any{})
	e.gw.Publish("s-short", "message.delta", map[string]any{kText: "Hello "})
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "s-short", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e.gw.Publish("s-short", "message.delta", map[string]any{kText: "world"})
	var streamed strings.Builder
	for _, ev := range until(t, ch, hub.KindMessageDelta) {
		if ev.Kind == hub.KindMessageDelta {
			streamed.WriteString(text(t, ev))
		}
	}
	if streamed.String() != "Hello " {
		t.Fatalf("first delta %q", streamed.String())
	}
	if ev := until(t, ch, hub.KindMessageDelta); text(t, ev[len(ev)-1]) != "world" {
		t.Fatalf("second delta %s", ev[len(ev)-1].Data)
	}
}

// joinLate opens a session whose running turn Hermes can no longer replay
// in full, and returns its events after the snapshot.
func joinLate(t *testing.T, e *env, id string) <-chan hub.Event {
	t.Helper()
	e.gw.ReplayMax = 8
	e.gw.AddSession(&gatewaytest.Session{ID: id, Live: true, Running: true})
	e.gw.Publish(id, "message.start", map[string]any{})
	for range 10 {
		e.gw.Publish(id, "tool.progress", map[string]any{kToolID: "t1"})
	}
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", ev.Kind)
	}
	return ch
}

// The interim that ends a held reply carries its whole text and reads as
// not streamed, so the page shows it; the next reply streams again.
func TestLateJoinInterimShowsTheWholeReply(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ch := joinLate(t, e, "s-interim")
	e.gw.Publish("s-interim", "message.delta", map[string]any{kText: "rest of it"})
	e.gw.Publish("s-interim", "message.interim", map[string]any{kText: "start and rest of it", "already_streamed": true})
	e.gw.Publish("s-interim", "message.delta", map[string]any{kText: "next"})
	evs := until(t, ch, hub.KindMessageDelta)
	if got := kinds(evs); !slices.Equal(got, []string{hub.KindMessageInterim, hub.KindMessageDelta}) {
		t.Fatalf("events %v", got)
	}
	var p struct {
		Text     string `json:"text"`
		Streamed bool   `json:"already_streamed"`
	}
	if err := json.Unmarshal(evs[0].Data, &p); err != nil || p.Text != "start and rest of it" || p.Streamed {
		t.Fatalf("interim %s", evs[0].Data)
	}
	if text(t, evs[1]) != "next" {
		t.Fatalf("delta %s", evs[1].Data)
	}
}

// Joined during a tool call, the reply after it starts after the join and
// streams as usual.
func TestLateJoinDuringAToolStreamsTheNextReply(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ch := joinLate(t, e, "s-tool")
	e.gw.Publish("s-tool", "tool.complete", map[string]any{kToolID: "t1", "name": "terminal"})
	e.gw.Publish("s-tool", "message.delta", map[string]any{kText: "All done."})
	evs := until(t, ch, hub.KindMessageDelta)
	if text(t, evs[len(evs)-1]) != "All done." {
		t.Fatalf("events %v", kinds(evs))
	}
}

// With a Hermes that hands the reply's text so far on resume (Talaria
// Hermes patch 0057), a chat that joins mid-reply shows the reply from its
// start and goes on streaming: deltas published right before the resume
// are in the seed, deltas right after it follow it, and frames the seed
// already covers that arrive again are dropped.
func TestLateJoinSeedsTheReplyAndKeepsStreaming(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{ViewerBuffer: 1 << 15})
	e.gw.ReplayMax = 512
	e.gw.InflightReply = true
	e.gw.ResendOnResume = 3
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-seed", Live: true, Running: true,
		Messages: []map[string]any{{kRole: roleUser, kText: "make the page"}}})
	reply := pageReply(60 << 10)
	parts := deltas(reply, 12)
	cut := len(parts) / 6
	e.gw.Publish("s-seed", "message.start", map[string]any{})
	for _, d := range parts[:cut] {
		e.gw.Publish("s-seed", "message.delta", map[string]any{kText: d})
	}
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "s-seed", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", ev.Kind)
	}
	seed := next(t, ch)
	if seed.Kind != hub.KindMessageDelta || text(t, seed) != strings.Join(parts[:cut], "") {
		t.Fatalf("seed %s of %d bytes", seed.Kind, len(text(t, seed)))
	}
	for _, d := range parts[cut:] {
		e.gw.Publish("s-seed", "message.delta", map[string]any{kText: d})
	}
	e.gw.FinishReply("s-seed", reply)
	streamed := text(t, seed)
	for _, ev := range until(t, ch, hub.KindMessageComplete) {
		if ev.Kind == hub.KindMessageDelta {
			streamed += text(t, ev)
		}
		if ev.Kind == "status.update" {
			t.Fatal("held the reply back though Hermes gave its start")
		}
	}
	if streamed != reply {
		t.Fatalf("streamed %d bytes, want the reply's %d once", len(streamed), len(reply))
	}
}

// midTurn runs a turn of tools tool calls, each with its result, a reply
// between them, and the start of a last reply, n deltas, on session id.
func midTurn(e *env, id string, tools int) (toolIDs []string, replySoFar string) {
	e.gw.Publish(id, "message.start", map[string]any{})
	for i := range tools {
		tid := "t" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + strings.Repeat("0", i/26)
		toolIDs = append(toolIDs, tid)
		e.gw.Publish(id, "tool.start", map[string]any{kToolID: tid, kName: toolTerm})
		for range 5 {
			e.gw.Publish(id, "tool.progress", map[string]any{kToolID: tid, "message": "running"})
		}
		e.gw.Publish(id, "tool.complete", map[string]any{kToolID: tid, kName: toolTerm, "result": "ok"})
		for k := range 10 {
			e.gw.Publish(id, "message.delta", map[string]any{kText: "step " + string(rune('0'+k)) + " "})
		}
		e.gw.Publish(id, "message.interim", map[string]any{kText: strings.Repeat("x", 3), "already_streamed": true})
	}
	for _, d := range []string{"The answer ", "so far "} {
		e.gw.Publish(id, "message.delta", map[string]any{kText: d})
		replySoFar += d
	}
	return toolIDs, replySoFar
}

// checkWholeTurn reads a viewer's events up to the turn's end: every tool
// card of the turn once, the reply so far, then the live rest, no status
// that holds the reply back, nothing twice.
func checkWholeTurn(t *testing.T, e *env, ch <-chan hub.Event, id string, toolIDs []string, soFar string) {
	t.Helper()
	e.gw.Publish(id, "message.delta", map[string]any{kText: "and the end."})
	e.gw.FinishReply(id, "whole")
	var got []string
	last := ""
	for _, ev := range until(t, ch, hub.KindMessageComplete) {
		switch ev.Kind {
		case hub.KindToolComplete:
			var p struct {
				ToolID string `json:"tool_id"`
			}
			_ = json.Unmarshal(ev.Data, &p)
			got = append(got, p.ToolID)
		case hub.KindMessageDelta:
			last += text(t, ev)
		case hub.KindMessageInterim:
			last = ""
		case "status.update":
			if strings.Contains(string(ev.Data), "late") {
				t.Fatalf("a status held the reply back: %s", ev.Data)
			}
		case hub.KindSnapshot:
			t.Fatal("a second snapshot")
		}
	}
	if !slices.Equal(got, toolIDs) {
		t.Fatalf("tool cards %d, want %d once each in order", len(got), len(toolIDs))
	}
	if last != soFar+"and the end." {
		t.Fatalf("reply %q, want %q", last, soFar+"and the end.")
	}
}

// A chat opened mid-turn on a hub that does not hold it (cold), after
// Hermes's replay ring dropped the turn's start, shows the whole turn so
// far from the snapshot's inflight turn (Talaria Hermes patch 0063), then
// goes on live.
func TestColdOpenMidTurnShowsTheWholeTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{ViewerBuffer: 1 << 15})
	e.gw.ReplayMax = 16
	e.gw.InflightTurn = true
	e.gw.AddSession(&gatewaytest.Session{ID: "s-cold", Live: true, Running: true,
		Messages: []map[string]any{{kRole: roleUser, kText: "do it"}}})
	tools, soFar := midTurn(e, "s-cold", 30)
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, "s-cold", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", ev.Kind)
	}
	checkWholeTurn(t, e, ch, "s-cold", tools, soFar)
}

// The same on a hub that held the chat (warm) but whose own buffer dropped
// the turn's start: the fresh snapshot it takes has the turn so far.
func TestWarmOpenMidTurnShowsTheWholeTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{ViewerBuffer: 1 << 15, RingSize: 32})
	e.gw.ReplayMax = 16
	e.gw.InflightTurn = true
	e.gw.AddSession(&gatewaytest.Session{ID: "s-warm", Live: true,
		Messages: []map[string]any{{kRole: roleUser, kText: "earlier"}, {kRole: "assistant", kText: "finished"}}})
	first, stop1, err := e.svc.Attach(context.Background(), userAlice, backendPC, "s-warm", 0)
	if err != nil {
		t.Fatal(err)
	}
	until(t, first, hub.KindSnapshot)
	stop1()
	e.gw.SetRunning("s-warm", true)
	tools, soFar := midTurn(e, "s-warm", 30)
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, "s-warm", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// The held snapshot shows at once; the fresh one replaces it.
	for _, want := range []bool{true, false} {
		ev := next(t, ch)
		for ev.Kind != hub.KindSnapshot {
			ev = next(t, ch)
		}
		if ev.Provisional != want {
			t.Fatalf("snapshot provisional = %v, want %v", ev.Provisional, want)
		}
	}
	checkWholeTurn(t, e, ch, "s-warm", tools, soFar)
}
