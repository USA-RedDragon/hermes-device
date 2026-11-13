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
