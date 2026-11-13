package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// plain.js turns an id that reaches the page into words, so no snake_case
// or dotted key shows; testdata/plain/run.js checks it.
func TestPlainScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "plain", "run.js"), filepath.Join("static", "plain.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
