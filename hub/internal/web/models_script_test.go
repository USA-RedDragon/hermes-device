package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// models.js reads Hermes's setup answers (address tests, key checks,
// sign-in polls, the model window) into what the Connect a model dialog
// shows, and walks the dialog's steps; testdata/models/run.js checks both
// with answers a real Hermes gave.
func TestModelsScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "models", "run.js"), filepath.Join("static", "models.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
