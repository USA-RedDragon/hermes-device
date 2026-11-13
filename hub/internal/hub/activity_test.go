package hub_test

import (
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

func TestIsBackground(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"bg_be5bed":                  true,
		"bg_124526_be5bed":           true,
		"20261111_031500_a1b2c3":     false,
		"cron_job1_20261111_031500":  false,
		"api_1762852500_0123abcd":    false,
		"":                           false,
		"xbg_be5bed":                 false,
		"proc_abc":                   false,
		"btw_be5bed":                 false,
		"6f1c2a9e-1d2b-4c3d-8e4f-01": false,
	} {
		if got := hub.IsBackground(id); got != want {
			t.Errorf("IsBackground(%q) = %v, want %v", id, got, want)
		}
	}
}
