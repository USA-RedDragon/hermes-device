package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// attach.js reads the attachment lines of user messages; testdata/attach
// holds the texts and what the page must make of them.
func TestAttachScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "attach", "run.js"), filepath.Join("static", "attach.js"),
		filepath.Join("testdata", "attach", "cases.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
