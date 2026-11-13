package models_test

import (
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/models"
)

const (
	slugDemo       = "talaria-demo"
	slugOpenRouter = "openrouter"
	providerAuto   = "auto"
	stockModel     = "anthropic/claude-opus-4.6"
)

func TestStatus(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	demo := models.Row{Slug: slugDemo, Authenticated: &yes, IsUserDefined: true}
	openrouter := models.Row{Slug: slugOpenRouter, Authenticated: &yes}
	for _, c := range []struct {
		name     string
		model    string
		provider string
		rows     []models.Row
		want     bool
	}{
		{"no model", "", providerAuto, []models.Row{openrouter}, false},
		// The agent's stock config on a new all-in-one install: a model, provider auto and no key. The demo
		// model's server is listed, but auto never picks it.
		{"stock default, only the demo server", stockModel, providerAuto, []models.Row{demo}, false},
		{"stock default, no providers", stockModel, providerAuto, nil, false},
		{"auto with a key", stockModel, providerAuto, []models.Row{demo, openrouter}, true},
		{"auto, current server", "m", "", []models.Row{{Slug: "x", Authenticated: &yes, IsUserDefined: true, IsCurrent: true}}, true},
		{"the demo model picked", slugDemo, slugDemo, []models.Row{demo}, true},
		{"named provider lost its key", "m", slugOpenRouter, []models.Row{{Slug: slugOpenRouter, Authenticated: &no}}, false},
		{"named provider not listed, a server is", "m", "custom", []models.Row{demo}, true},
	} {
		if got := models.Ready(c.model, c.provider, c.rows...); got != c.want {
			t.Errorf("%s: ready = %v, want %v", c.name, got, c.want)
		}
	}
}
