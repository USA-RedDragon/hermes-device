package web_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// Typing in the message box moves nothing and a touch screen never gets the
// keyboard raised by opening a chat; testdata/typing/run.js drives the real
// page in headless Chrome. It skips where node or Chrome is missing.
func TestTypingMovesNothing(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	var chrome string
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			chrome = p
			break
		}
	}
	if chrome == "" {
		t.Skip("Chrome is not installed")
	}
	e := setup(t)
	ms := make([]map[string]any, 0, 80)
	for i := range 40 {
		ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("question %d", i)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("Answer %d fills the log. ", i), 8)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Long", Live: true, StartedAt: 900, Messages: ms})
	e.gw.AddSession(&gatewaytest.Session{ID: "s8", Title: "Second", Live: true, StartedAt: 800, Messages: ms[:4]})
	prof, err := os.MkdirTemp("", "typing-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "typing", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
