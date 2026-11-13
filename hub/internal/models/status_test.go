package models

import "testing"

func TestStatus(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	demo := optionsRow{Slug: "talaria-demo", Authenticated: &yes, IsUserDefined: true}
	openrouter := optionsRow{Slug: "openrouter", Authenticated: &yes}
	for _, c := range []struct {
		name string
		o    options
		want bool
	}{
		{"no model", options{Provider: "auto", Providers: []optionsRow{openrouter}}, false},
		// The agent's stock config on a new all-in-one install: a model, provider auto and no key. The demo
		// model's server is listed, but auto never picks it.
		{"stock default, only the demo server", options{Model: "anthropic/claude-opus-4.6", Provider: "auto", Providers: []optionsRow{demo}}, false},
		{"stock default, no providers", options{Model: "anthropic/claude-opus-4.6", Provider: "auto"}, false},
		{"auto with a key", options{Model: "anthropic/claude-opus-4.6", Provider: "auto", Providers: []optionsRow{demo, openrouter}}, true},
		{"auto, current server", options{Model: "m", Provider: "", Providers: []optionsRow{{Slug: "x", Authenticated: &yes, IsUserDefined: true, IsCurrent: true}}}, true},
		{"the demo model picked", options{Model: "talaria-demo", Provider: "talaria-demo", Providers: []optionsRow{demo}}, true},
		{"named provider lost its key", options{Model: "m", Provider: "openrouter", Providers: []optionsRow{{Slug: "openrouter", Authenticated: &no}}}, false},
		{"named provider not listed, a server is", options{Model: "m", Provider: "custom", Providers: []optionsRow{demo}}, true},
	} {
		if got := status(c.o).Ready; got != c.want {
			t.Errorf("%s: ready = %v, want %v", c.name, got, c.want)
		}
	}
}
