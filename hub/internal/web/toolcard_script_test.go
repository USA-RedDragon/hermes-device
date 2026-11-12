package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// toolcard.js derives the icon, title and status of a tool call's card;
// testdata/toolcard holds the calls and what the card must say.
func TestToolCardScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "toolcard", "run.js"), filepath.Join("static", "toolcard.js"),
		filepath.Join("testdata", "toolcard", "cases.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
