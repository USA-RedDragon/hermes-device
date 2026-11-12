package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func runStreamScript(t *testing.T, script string, files ...string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	args := make([]string, 0, 1+len(files))
	args = append(args, filepath.Join("testdata", "stream", script))
	for _, f := range files {
		args = append(args, filepath.Join("static", f))
	}
	out, err := exec.CommandContext(t.Context(), node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The event stream owner waits longer after each stream that dies young,
// shows its banner after 5 seconds without a healthy stream and clears it
// when one is back, and checks the hub's version once, not per reconnect.
func TestEventStreamScript(t *testing.T) {
	t.Parallel()
	runStreamScript(t, "stream.js", "eventstream.js")
}

// A message the page sends again after a lost answer carries the same ID.
func TestSendScript(t *testing.T) {
	t.Parallel()
	runStreamScript(t, "send.js", "eventstream.js", "send.js")
}

// A snapshot that arrives after part of the page was drawn keeps the
// messages still being sent, maps the others by the ID they were sent
// under, and keeps the reader's place.
func TestReconcileScript(t *testing.T) {
	t.Parallel()
	runStreamScript(t, "reconcile.js", "reconcile.js")
}

// The log stays at its end for a reader at the end through any change of
// its size, and on the same message for a reader above it; a scroll the
// layout caused is not the reader.
func TestStickScript(t *testing.T) {
	t.Parallel()
	runStreamScript(t, "stick.js", "stick.js")
}

// What the reader opened or closed in the chat stays so when the page
// draws the nodes again.
func TestUIStateScript(t *testing.T) {
	t.Parallel()
	runStreamScript(t, "uistate.js", "uistate.js")
}

// A model pick that waited for the end of a turn can come back as a
// question or a refusal; the page reads both.
func TestModelPickScript(t *testing.T) {
	t.Parallel()
	runStreamScript(t, "modelpick.js", "modelpick.js")
}
