package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// activity.js draws the subagent rows of the Activity sidebar: the status
// line as a sentence with the raw text one tap away, the token line, and
// whether a row runs or ended, which only Hermes decides.
func TestActivityScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "activity", "run.js"), "static").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
