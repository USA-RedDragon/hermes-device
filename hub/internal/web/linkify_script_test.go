package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// linkify.js links the bare addresses of a plain text; testdata/linkify/run.js
// checks the trailing punctuation, the brackets, the schemes and a hostile text.
func TestLinkifyScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "linkify", "run.js"), filepath.Join("static", "linkify.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
