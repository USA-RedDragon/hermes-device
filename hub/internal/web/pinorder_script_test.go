package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// pinorder.js decides what a drop of a chat row does; testdata/pinorder/run.js
// checks pin at a place, unpin, move and cancel.
func TestPinOrderScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pinorder", "run.js"), filepath.Join("static", "pinorder.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
