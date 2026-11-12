package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// times.js writes the times of a chat; testdata/times/run.js checks today
// against an earlier day, the time zone, run lengths and the live counter.
func TestTimesScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "times", "run.js"), filepath.Join("static", "times.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
