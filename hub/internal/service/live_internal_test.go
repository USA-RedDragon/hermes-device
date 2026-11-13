package service

import (
	"encoding/json"
	"testing"
)

func TestPlainThinking(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"(◔_◔) brainstorming...":   "Brainstorming...",
		"( ͡° ͜ʖ ͡°) pondering...": "Pondering...",
		"( •_•)>⌐■-■ musing...":    "Musing...",
		"ヽ(>∀<☆)☆ reasoning...":    "Reasoning...",
		"٩(๑❛ᴗ❛๑)۶ analyzing...":   "Analyzing...",
		"(⚔) forging...":           "Forging...",
		"compressing context":      "Compressing context",
		"Waiting for 2 tools":      "Waiting for 2 tools",
		"":                         "",
	} {
		raw, err := json.Marshal(map[string]string{textKey: in})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(plainThinking(raw), &got); err != nil || got[textKey] != want {
			t.Errorf("plainThinking(%q) = %q, want %q", in, got[textKey], want)
		}
	}
}

func TestViewRowsDropsUnshownFields(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi","row_id":3,"message_uid":"u1","_db_persisted":true}`),
		json.RawMessage(`{"role":"assistant","content":"","reasoning":"r","reasoning_details":[{"data":"x"}],"tool_calls":[{"id":"c"}]}`),
		json.RawMessage(`{"role":"tool","content":"says \"reasoning_details\" in text","tool_call_id":"c"}`),
		json.RawMessage(`{"role":"user","content":"plain"}`),
		json.RawMessage(`not json "message_uid"`),
	}
	got := viewRows(rows)
	want := []string{
		`{"content":"hi","role":"user","row_id":3}`,
		`{"content":"","reasoning":"r","role":"assistant","tool_calls":[{"id":"c"}]}`,
		`{"role":"tool","content":"says \"reasoning_details\" in text","tool_call_id":"c"}`,
		`{"role":"user","content":"plain"}`,
		`not json "message_uid"`,
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("row %d = %s, want %s", i, got[i], want[i])
		}
	}
	if out, err := json.Marshal(viewRows(nil)); err != nil || string(out) != "[]" {
		t.Errorf("no rows = %s %v", out, err)
	}
}
