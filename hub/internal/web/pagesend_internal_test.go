package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// pagesend.js marks and reads the line that names a page on its answers
// and delivers them once; the page's own bridge (hermes.send, hermes.draft)
// is run against a fake port.
func TestPageSendScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	bridge := filepath.Join(t.TempDir(), "bridge.html")
	if err := os.WriteFile(bridge, withBridge([]byte("<p>x</p>")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pagesend", "run.js"), filepath.Join("static", "pagesend.js"), bridge).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
