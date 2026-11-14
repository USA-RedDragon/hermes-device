package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// toast.js is the page's one toast. testdata/toast/run.js drives its model
// with a fake clock: the time a toast stays (4 s and 60 ms a word, double for
// an error), the name clamp, at most 3 shown with the oldest dropped first,
// identical text refreshing the toast, and the pause on hover or focus.
func TestToastScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "toast", "run.js"), filepath.Join("static", "toast.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A toast sits under the header at the right (centered and full width on a
// phone), never over the composer or the send button, at 412 and 1280 pixels,
// and waits while the pointer is on it. testdata/toast/browser.js drives the
// real page in headless Chrome. It skips where node or Chrome is missing.
func TestToastPlacementAndHoverChrome(t *testing.T) {
	t.Parallel()
	bin := chromePath(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := setup(t)
	prof, err := os.MkdirTemp("", "toast-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "toast", "browser.js"), bin, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
