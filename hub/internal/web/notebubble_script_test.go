package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// A message the user typed shows without the notes Hermes added around it
// for the model, though it carries a client's submission ID;
// testdata/notebubble/run.js drives the real page in headless Chrome.
// NOTEBUBBLE_SHOT=prefix also writes screenshots. It skips where node or
// Chrome is missing.
func TestTypedRowShowsWithoutHermesNotes(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Work", Live: true, StartedAt: 900,
		Messages: []map[string]any{
			{kRole: roleUser, kText: "[Note: model was just switched from gpt-5 to claude-sonnet-5-5 via anthropic. Adjust your self-identification accordingly.]\n\nis this a bug?", "submission_id": "web-1"},
			{kRole: roleAssistant, kText: "Let me look."},
			{kRole: roleUser, kText: "ping\n\n[Waiting: to wait for something slow, start it with terminal(background=true), then call process_manage(action='wait', session_id=..., timeout=...).]", "submission_id": "web-2"},
			{kRole: roleAssistant, kText: "pong"},
		}})
	prof, err := os.MkdirTemp("", "notebubble-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	args := []string{filepath.Join("testdata", "notebubble", "run.js"), chrome, e.url, prof}
	if p := os.Getenv("NOTEBUBBLE_SHOT"); p != "" {
		args = append(args, p)
	}
	out, err := exec.CommandContext(t.Context(), node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
