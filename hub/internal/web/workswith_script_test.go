package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// workswith.js shows the chats marked as working together: the menu row
// and its count, the panel with the partners and the picker, the chips on
// agent message cards and the link under a refusal. testdata/workswith/run.js
// runs the helpers and the panel against a small DOM and a fake API.
func TestWorksWithScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "workswith", "run.js"), filepath.Join("static", "workswith.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
