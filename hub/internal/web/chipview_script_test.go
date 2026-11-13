package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

const chromeName = "chrome"

// A pending image chip in the composer opens the image viewer and Back or
// Escape close it without leaving an overlay; testdata/chipview/run.js drives
// the real page in headless Chrome, touch and desktop. It skips where node or
// Chrome is missing.
func TestChipOpensViewer(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	var chrome string
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", chromeName} {
		if p, err := exec.LookPath(n); err == nil {
			chrome = p
			break
		}
	}
	if chrome == "" {
		t.Skip("Chrome is not installed")
	}
	e := setup(t)
	e.gw.QueueWhileRunning = true
	const waiting = "/opt/data/hermes/cache/images/waiting.png"
	e.gw.SetFile(waiting, []byte("\x89\x50\x4e\x47\x0d\x0a\x1a\x0a\x00\x00\x00\x0d\x49\x48\x44\x52\x00\x00\x00\x0a\x00\x00\x00\x0a\x08\x06\x00\x00\x00\x8d\x32\xcf\xbd\x00\x00\x00\x15\x49\x44\x41\x54\x78\xda\x63\xfc\xcf\xc0\xf0\x9f\x81\x08\xc0\x38\xaa\x90\xbe\x0a\x01\xc4\xfd\x18\xed\x14\xe0\x1c\x68\x00\x00\x00\x00\x49\x45\x4e\x44\xae\x42\x60\x82"))
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Work", Live: true, Running: true, StartedAt: 900,
		Pending: []gateway.PendingAdmission{{AdmissionID: "a1", Sequence: 1, Status: "queued", Text: "after this\n\n[Image attached at: " + waiting + "]"}}, Messages: []map[string]any{
			{kRole: roleUser, kText: "start\n\n[Image attached at: " + waiting + "]"}, {kRole: roleAssistant, kText: "working"}}})
	prof, err := os.MkdirTemp("", "chipview-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "chipview", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
