//go:build linux

package acphost

import "testing"

func TestParseStat(t *testing.T) {
	t.Parallel()
	info, ok := parseStat("123 (we ir) d) S 45 67 67 0 -1")
	if !ok || info.ppid != 45 || info.pgid != 67 || info.zombie {
		t.Errorf("parseStat = %+v %v", info, ok)
	}
	if info, ok := parseStat("9 (x) Z 1 9 9"); !ok || !info.zombie {
		t.Errorf("zombie = %+v %v", info, ok)
	}
}
