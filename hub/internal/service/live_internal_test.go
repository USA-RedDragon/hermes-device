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
		raw, err := json.Marshal(map[string]string{"text": in})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(plainThinking(raw), &got); err != nil || got["text"] != want {
			t.Errorf("plainThinking(%q) = %q, want %q", in, got["text"], want)
		}
	}
}
