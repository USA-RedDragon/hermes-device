package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// A page_publish call of a chat shows the page card with an Open button
// that opens the page, in place of a generic tool card with a raw path, and
// the reply that names the same file shows no second card;
// testdata/pagepub/run.js drives the real page in headless Chrome. It skips
// where node or Chrome is missing. PAGEPUB_SHOT is a path prefix for
// screenshots.
func TestPublishedPageCardOpens(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setupWith(t, func(cfg *config.Config, _ *web.Options, _ *store.Store) {
		cfg.Backends[0].MediaRoots = []string{"/opt/data/hermes/cache"}
	})
	file := "/opt/data/hermes/cache/pages/hermes-device/pp1/page-0123456789abcdef.html"
	e.gw.SetFile(file, []byte("<!doctype html><title>Tracker</title><p>Hello</p>"))
	result := `{"ok": true, "path": "` + file + `", "bytes": 49, "title": "Tracker", "show_user": "MEDIA:` + file + `"}`
	e.gw.AddSession(&gatewaytest.Session{ID: "pp1", Title: "Published page", Live: true, StartedAt: 900, Messages: []map[string]any{
		{kRole: roleUser, kText: "make me a tracker page"},
		{kRole: roleAssistant, kText: "", kCalls: []map[string]any{{"id": "c1", fnKey: map[string]any{fieldName: "page_publish", ilArgs: `{"path": "/home/u/tracker.html"}`}}}},
		{kRole: stepTool, kCallID: "c1", "content": result},
		{kRole: roleAssistant, kText: "Done.\nMEDIA:" + file},
	}})
	prof, err := os.MkdirTemp("", "pagepub-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pagepub", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
