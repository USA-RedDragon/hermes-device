package web_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// A click on a chat in the list opens it even when the list is drawn
// again between the press and the release. The page fetches the list every
// few seconds and after chat events, and a list that replaced its rows
// then sent the click to the list, not to the chat: the reader had to
// click twice. testdata/rowpress/run.js presses on a row, draws the list
// again (unchanged, then with the row moving), and releases, in headless
// Chrome and Firefox. Each skips where node or its browser is missing.
func TestChatClickSurvivesListRedrawChrome(t *testing.T) {
	t.Parallel()
	rowPress(t, chromePath(t))
}

func TestChatClickSurvivesListRedrawFirefox(t *testing.T) {
	t.Parallel()
	rowPress(t, firefoxPath(t))
}

func rowPress(t *testing.T, bin string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := setup(t)
	for i, id := range []string{"s2", "s3", "s4"} {
		e.gw.AddSession(&gatewaytest.Session{ID: id, Title: "Chat " + id, Live: true, StartedAt: float64(90 - i),
			Messages: []map[string]any{{kRole: roleUser, kText: "hello " + id}, {kRole: roleAssistant, kText: fmt.Sprintf("reply %s", id)}}})
	}
	prof, err := os.MkdirTemp("", "rowpress-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "rowpress", "run.js"), bin, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
