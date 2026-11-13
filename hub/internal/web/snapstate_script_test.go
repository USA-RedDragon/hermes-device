package web_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// What the reader opened in a chat (a tool group, a tool card in it, a
// thinking block) stays open when a reconnect draws the chat again from a
// snapshot; testdata/snapstate/run.js drives the real page in headless
// Chrome. SNAPSTATE_SHOT=prefix also writes a screenshot. It skips where
// node or Chrome is missing.
func TestOpenStateSurvivesASnapshot(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	call := func(id, cmd string) map[string]any {
		return map[string]any{"id": id, "function": map[string]any{fieldName: "terminal", "arguments": `{"command":"` + cmd + `"}`}}
	}
	result := func(id, out string) map[string]any {
		return map[string]any{kRole: "tool", kCallID: id, fieldName: "terminal", kText: `{"output": "` + out + `", "exit_code": 0}`}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Work", Live: true, StartedAt: 900,
		Messages: []map[string]any{
			{kRole: roleUser, kText: "look around"},
			{kRole: roleAssistant, kText: "", "reasoning": "I will list the folder first and then read the file that matters here.",
				kCalls: []any{call("c1", "ls"), call("c2", "pwd")}},
			result("c1", "a b c"),
			result("c2", "/work"),
			{kRole: roleAssistant, kText: "The folder has three files."},
			{kRole: roleUser, kText: "and the second one"},
			{kRole: roleAssistant, kText: "", kCalls: []any{call("c3", "cat b"), call("c4", "cat c")}},
			result("c3", "bee"),
			result("c4", "sea"),
			{kRole: roleAssistant, kText: "It says bee."},
		}})
	poke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e.gw.RetireSubscriptions("s9")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(poke.Close)
	prof, err := os.MkdirTemp("", "snapstate-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	args := []string{filepath.Join("testdata", "snapstate", "run.js"), chrome, e.url, prof, poke.URL}
	if p := os.Getenv("SNAPSTATE_SHOT"); p != "" {
		args = append(args, p)
	}
	out, err := exec.CommandContext(t.Context(), node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
