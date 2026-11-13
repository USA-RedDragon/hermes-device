package setup

import (
	"os"
	"testing"
)

// /dev/null is a character device but no terminal: setup run with
// stdin from it (launchd, cron, ssh host cmd < /dev/null) must not prompt.
func TestIsTerminalDevNull(t *testing.T) {
	t.Parallel()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("isTerminal(%s) = true", os.DevNull)
	}
	p, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if isTerminal(p) {
		t.Error("isTerminal(a plain file) = true")
	}
}
