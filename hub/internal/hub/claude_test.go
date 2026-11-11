package hub_test

import (
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func TestClaudeModelName(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{
		"claude-opus-5-5[1m]":       "Opus 5.5",
		"claude-sonnet-5-5":         "Sonnet 5.5",
		"claude-fable-5-2":          "Fable 5.2",
		"claude-haiku-4-5-20251001": "Haiku 4.5",
		"claude-opus-5":             "Opus 5",
		"claude-opus":               "",
		"claude-":                   "",
		"claude-5-5":                "",
		"claude-opus-x":             "",
		"qwen3.8:27b":               "",
	} {
		got, ok := hub.ClaudeModelName(id)
		if got != want || ok != (want != "") {
			t.Errorf("ClaudeModelName(%q) = %q, %v; want %q", id, got, ok, want)
		}
	}
}

func TestArrangeModels(t *testing.T) {
	t.Parallel()
	const haiku = "claude-haiku-5-5"
	ps := []hub.ModelProvider{
		{ID: "custom:local", Name: "local", Models: []hub.ModelChoice{{ID: "qwen"}}},
		{ID: "claude-api-caam", Name: "Claude API (caam)", Models: []hub.ModelChoice{{ID: "claude-opus-5-5"}}},
		{ID: hub.ClaudePoolProvider, Name: "Claude", Models: []hub.ModelChoice{{ID: "claude-sonnet-5-5[1m]"}}},
		{ID: "claude-api-jaam", Name: "Claude API (jaam)", Models: []hub.ModelChoice{{ID: haiku}}},
		{ID: "claude-api-", Name: "odd", Models: []hub.ModelChoice{{ID: haiku}}},
	}
	hub.ArrangeModels(ps)
	want := [][3]string{
		{hub.ClaudePoolProvider, "Claude", "Sonnet 5.5"},
		{"claude-api-caam", "Claude API credits", "Opus 5.5 (caam credits)"},
		{"claude-api-jaam", "Claude API credits", "Haiku 5.5 (jaam credits)"},
		{"custom:local", "local", "qwen"},
		{"claude-api-", "odd", haiku},
	}
	for i, w := range want {
		if p := ps[i]; p.ID != w[0] || p.Group != w[1] || p.Models[0].Label != w[2] {
			t.Errorf("provider %d = %s %q %q, want %v", i, p.ID, p.Group, p.Models[0].Label, w)
		}
	}
}
