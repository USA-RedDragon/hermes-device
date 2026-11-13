package web

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// pageblocks.js turns a page block into a card as soon as its closing fence
// streams in, and only a block still open shows the wait note.
func TestPageBlocksScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pageblocks", "run.js"), filepath.Join("static", "pageblocks.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
