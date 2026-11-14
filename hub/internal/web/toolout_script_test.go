package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// A chat opens with its long tool outputs cut. Closed cards build no output
// block and fetch nothing; opening one fetches its output once and shows it
// whole. testdata/toolout/run.js drives the real page in headless Chrome.
func TestToolOutputLoadsWhenACardOpens(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	const tail = "A LINE ONLY THE FULL OUTPUT HAS"
	// The card shows at most 20,000 characters, so the line is early.
	whole := strings.Repeat("line of build output\n", 400) + tail + "\n" + strings.Repeat("line of build output\n", 2600)
	call := func(id string) map[string]any {
		return map[string]any{"id": id, fnKey: map[string]any{fieldName: toolTerminal, ilArgs: `{"command":"make"}`}}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s8", Title: "Cut", Live: true, StartedAt: 800, Messages: []map[string]any{
		{kRole: roleUser, kText: "build it"},
		{kRole: roleAssistant, kText: "", kCalls: []any{call("a1"), call("a2"), call("a3")}},
		{kRole: stepTool, kCallID: "a1", fieldName: toolTerminal, kText: `{"output": ` + mustJSON(t, whole) + `, "exit_code": 0}`},
		{kRole: stepTool, kCallID: "a2", fieldName: toolTerminal, kText: `{"output": "ok", "exit_code": 0}`},
		{kRole: stepTool, kCallID: "a3", fieldName: toolTerminal, kText: `{"output": ` + mustJSON(t, whole+"2") + `, "exit_code": 0}`},
		{kRole: roleAssistant, kText: "finished"},
	}})
	prof, err := os.MkdirTemp("", "toolout-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "toolout", "run.js"), chrome, e.url, prof, tail).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
