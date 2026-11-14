package web_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// A browser_batch call is one card (steps in plain words, then the page
// read), and a single browser card names its element by the label the device
// plugin sent, never by a ref. testdata/browsercards/session.json is the chat
// (the results carry the plugin's details and target); run.js drives the real
// page in headless Chrome, phone and desktop, light and dark. BROWSERCARDS_SHOT=
// prefix also writes screenshots. It skips where node or Chrome is missing.
func TestBrowserCardsNameElementsAndFoldBatches(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	raw, err := os.ReadFile(filepath.Join("testdata", "browsercards", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Browser", Live: true, StartedAt: 900, Messages: msgs})
	prof, err := os.MkdirTemp("", "browsercards-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "browsercards", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
