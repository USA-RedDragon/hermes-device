package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// attach.js drops the notes that Hermes appends to a user message; the
// hub's testdata/notes holds the texts and what is left, as the Go side
// reads them.
func TestNotesScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "notes", "run.js"), filepath.Join("static", "attach.js"),
		filepath.Join("..", "hub", "testdata", "notes", "cases.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
