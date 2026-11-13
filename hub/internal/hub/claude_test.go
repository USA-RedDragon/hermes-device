package hub_test

import (
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const sonnet = "Sonnet 5.5"

func TestClaudeModelName(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{
		"claude-opus-5-5[1m]":       "Opus 5.5",
		"claude-sonnet-5-5":         sonnet,
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
		{hub.ClaudePoolProvider, "Claude", sonnet},
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

func TestArrangeModelsNewestPerFamily(t *testing.T) {
	t.Parallel()
	ids := []string{"claude-sonnet-5-5[1m]", "claude-opus-5-5[1m]", "claude-fable-5-1[1m]", "claude-haiku-5-5[1m]",
		"claude-fable-5-2", "claude-haiku-4-5-20251001", "claude-opus-4-8[1m]", "claude-mythos-5-1[1m]", "default"}
	p := hub.ModelProvider{ID: hub.ClaudePoolProvider}
	for _, id := range ids {
		p.Models = append(p.Models, hub.ModelChoice{ID: id})
	}
	ps := []hub.ModelProvider{p}
	hub.ArrangeModels(ps)
	got := make([]string, 0, len(ps[0].Models))
	for _, m := range ps[0].Models {
		got = append(got, m.Label)
	}
	want := []string{"Opus 5.5", "Sonnet 5.5", "Fable 5.2", "Haiku 5.5", "Mythos 5.1", "default"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pool models = %v, want %v", got, want)
	}
}
