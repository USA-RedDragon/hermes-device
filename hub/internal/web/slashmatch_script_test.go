package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// slashmatch.js ranks the commands the message box offers after a "/";
// testdata/slashmatch/run.js checks the order and the aliases.
func TestSlashMatchScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "slashmatch", "run.js"), filepath.Join("static", "slashmatch.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
