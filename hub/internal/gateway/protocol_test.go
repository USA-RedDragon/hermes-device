package gateway_test

import (
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
)

func TestProducer(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`["cron","agent:main:local:dm:local-1","ab"]`: "cron",
		`["completion","k","1"]`:                      "completion",
		"3f1c6c2e-8a8e-4c55-9d5e-4a1d3b1e2f00":        "",
		"":                                            "",
		"[":                                           "",
		"[]":                                          "",
		"[1]":                                         "",
	} {
		if got := gateway.Producer(in); got != want {
			t.Errorf("Producer(%q) = %q, want %q", in, got, want)
		}
	}
}
