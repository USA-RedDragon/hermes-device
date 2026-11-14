package agentcfg_test

import (
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
)

// The long-chat point is any 5% step from 30% to 90%.
func TestValidThreshold(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{0.30, 0.35, 0.5, 0.55, 0.7, 0.85, 0.9} {
		if !agentcfg.ValidThreshold(v) {
			t.Errorf("%v rejected", v)
		}
	}
	for _, v := range []float64{0, 0.25, 0.29, 0.33, 0.51, 0.875, 0.95, 1, 1.5, -0.5} {
		if agentcfg.ValidThreshold(v) {
			t.Errorf("%v accepted", v)
		}
	}
}
