package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// After a page opened from a card is left with the phone's Back button, taps
// still work as they do after its own Back button; testdata/pageback/run.js
// drives the real page in headless Chrome with touch. It skips where node or
// Chrome is missing.
func TestPageBackKeepsTaps(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	reply := "Here is the page.\n\n```page\n<!doctype html><title>Demo</title><style>p{height:900px}</style><p>Hello</p><input id=\"q\"><button>Pick</button><p>End</p>\n```\n"
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Pages", Live: true, StartedAt: 900, Messages: []map[string]any{
		{kRole: roleUser, kText: "make a page"}, {kRole: roleAssistant, kText: reply}}})
	e.gw.AddSession(&gatewaytest.Session{ID: "s8", Title: "Second", Live: true, StartedAt: 800, Messages: []map[string]any{{kRole: roleUser, kText: "hi"}}})
	prof, err := os.MkdirTemp("", "pageback-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pageback", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
