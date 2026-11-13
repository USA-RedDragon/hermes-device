package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const (
	kRole        = "role"
	kText        = "text"
	rowUser      = "user"
	rowAssistant = "assistant"
	headRows     = hub.HeadRows
	headBytes    = hub.HeadBytes
)

func rowRole(m json.RawMessage) string {
	var r struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(m, &r)
	return r.Role
}

func rows(turns, tools int) []map[string]any {
	ms := make([]map[string]any, 0, turns*(2*tools+2))
	for i := range turns {
		ms = append(ms, map[string]any{kRole: rowUser, kText: fmt.Sprintf("q%d", i)})
		for k := range tools {
			ms = append(ms, map[string]any{kRole: rowAssistant, kText: "", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("c%d-%d", i, k)}}},
				map[string]any{kRole: "tool", "tool_call_id": fmt.Sprintf("c%d-%d", i, k), "content": strings.Repeat("x", 2000)})
		}
		ms = append(ms, map[string]any{kRole: rowAssistant, kText: fmt.Sprintf("a%d", i)})
	}
	return ms
}

func snapOf(t *testing.T, ms []map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"session_id": "s", "title": "T", "messages": ms, "info": map[string]any{"cwd": "/w"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type splitView struct {
	Title    string            `json:"title"`
	Older    int               `json:"older"`
	Messages []json.RawMessage `json:"messages"`
	Info     map[string]any    `json:"info"`
}

// A big chat's snapshot goes as its newest whole turns, a small part of
// it, then the rest; together they are the snapshot, in order.
func TestSplitSnapshotNewestTurnsFirst(t *testing.T) {
	t.Parallel()
	ms := rows(900, 1)
	data := snapOf(t, ms)
	head, older, ok := splitSnapshot(data)
	if !ok {
		t.Fatal("a 900-turn chat was not split")
	}
	var h splitView
	var o struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(head, &h); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(older, &o); err != nil {
		t.Fatal(err)
	}
	if h.Title != "T" || h.Info["cwd"] != "/w" {
		t.Fatalf("head lost the snapshot's fields: %+v", h)
	}
	if h.Older != len(o.Messages) || len(o.Messages)+len(h.Messages) != len(ms) {
		t.Fatalf("older = %d, %d older rows and %d head rows, want %d in all", h.Older, len(o.Messages), len(h.Messages), len(ms))
	}
	if len(h.Messages) < headRows || len(head) > len(data)/10 {
		t.Fatalf("head has %d rows, %d of %d bytes", len(h.Messages), len(head), len(data))
	}
	if rowRole(h.Messages[0]) != rowUser {
		t.Fatal("the head does not start a turn")
	}
	all := append(append([]json.RawMessage{}, o.Messages...), h.Messages...)
	for i, m := range ms {
		want, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if string(all[i]) != string(want) {
			t.Fatalf("row %d differs", i)
		}
	}
}

// A small chat goes whole.
func TestSplitSnapshotSmallChatWhole(t *testing.T) {
	t.Parallel()
	if _, _, ok := splitSnapshot(snapOf(t, rows(5, 1))); ok {
		t.Fatal("a 5-turn chat was split")
	}
}

// One turn of hundreds of tool calls is cut at an assistant row, not sent
// whole.
func TestSplitSnapshotLongTurnCutAtAssistant(t *testing.T) {
	t.Parallel()
	ms := rows(2, 600)
	head, _, ok := splitSnapshot(snapOf(t, ms))
	if !ok {
		t.Fatal("not split")
	}
	var h splitView
	if err := json.Unmarshal(head, &h); err != nil {
		t.Fatal(err)
	}
	if rowRole(h.Messages[0]) != rowAssistant || len(head) > 4*headBytes+headBytes {
		t.Fatalf("head starts with %s and has %d bytes", rowRole(h.Messages[0]), len(head))
	}
}

type fpOut struct {
	buf strings.Builder
}

func (o *fpOut) Write(p []byte) (int, error) { return o.buf.Write(p) }

func fpServer() *Server { return &Server{instance: "i"} }

func snapWith(t *testing.T, ms []map[string]any, extra map[string]any) json.RawMessage {
	t.Helper()
	m := map[string]any{"session_id": "s", "title": "T", "messages": ms}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A page that reconnected to a replica that cannot resume its event id
// says which transcript it shows (?fp=). An idle chat with that same
// transcript gets a one-line "snapshot.same", not the whole chat again;
// a changed transcript, or a running turn, gets the snapshot.
func TestSnapshotSameWhenThePageShowsIt(t *testing.T) {
	t.Parallel()
	ms := rows(40, 1)
	data := snapWith(t, ms, nil)
	fp := snapFP(data)
	if fp == "" {
		t.Fatal("an idle chat has no fingerprint")
	}
	out := &fpOut{}
	ew := &eventWriter{s: fpServer(), split: true, fp: fp}
	if !ew.write(out, hub.Event{Seq: 7, Kind: hub.KindSnapshot, Data: data}) {
		t.Fatal("write failed")
	}
	got := out.buf.String()
	if !strings.Contains(got, `"kind":"snapshot.same"`) || strings.Contains(got, "q0") || len(got) > 200 {
		t.Fatalf("not a one-line answer (%d bytes): %.200s", len(got), got)
	}
	if !strings.Contains(got, "id: i.7\n") {
		t.Fatalf("the answer lost the event id: %s", got)
	}

	// The transcript changed since: the whole snapshot, with its new fingerprint.
	out = &fpOut{}
	ew = &eventWriter{s: fpServer(), fp: fp}
	data2 := snapWith(t, append(ms[:len(ms):len(ms)], map[string]any{kRole: rowUser, kText: "new"}), nil)
	ew.write(out, hub.Event{Seq: 8, Kind: hub.KindSnapshot, Data: data2})
	if !strings.Contains(out.buf.String(), `"kind":"snapshot"`) || !strings.Contains(out.buf.String(), `"fp":"`+snapFP(data2)+`"`) {
		t.Fatalf("a changed chat was not sent: %.200s", out.buf.String())
	}

	// A running turn, a waiting input or an open approval is live state
	// the page must take whole.
	for name, extra := range map[string]map[string]any{
		"running": {"running": true},
		"pending": {"pending": []any{map[string]any{"id": "a"}}},
		"prompt":  {"prompts": []any{map[string]any{"id": "p"}}},
	} {
		d := snapWith(t, ms, extra)
		if snapFP(d) != "" {
			t.Fatalf("%s: a live chat has a fingerprint", name)
		}
		out = &fpOut{}
		ew = &eventWriter{s: fpServer(), fp: fp}
		ew.write(out, hub.Event{Kind: hub.KindSnapshot, Data: d})
		if strings.Contains(out.buf.String(), "snapshot.same") {
			t.Fatalf("%s: answered with snapshot.same", name)
		}
	}
}

// The hub's copy of a chat goes out marked provisional, so a page that
// shows the chat can ignore it.
func TestProvisionalSnapshotIsMarked(t *testing.T) {
	t.Parallel()
	out := &fpOut{}
	ew := &eventWriter{s: fpServer()}
	ew.write(out, hub.Event{Kind: hub.KindSnapshot, Data: snapWith(t, rows(900, 1), nil), Provisional: true})
	if !strings.Contains(out.buf.String(), `"provisional":true`) {
		t.Fatalf("a held copy is not marked: %.200s", out.buf.String())
	}
	out = &fpOut{}
	ew = &eventWriter{s: fpServer()}
	ew.write(out, hub.Event{Kind: hub.KindSnapshot, Data: snapWith(t, rows(2, 1), nil), Provisional: true})
	if !strings.Contains(out.buf.String(), `"provisional":true`) {
		t.Fatalf("a small held copy is not marked: %.200s", out.buf.String())
	}
}
