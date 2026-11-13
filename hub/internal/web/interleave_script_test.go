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

const (
	ilArgs     = "arguments"
	ilThink    = "reasoning"
	ilDone     = "All done."
	ilComplete = "complete"
)

// A turn's thinking and tool cards show in the order they happened
// (thinking, card, thinking, cards, thinking, reply), live and when the
// chat loads from its stored rows; testdata/interleave/run.js drives the
// real page in headless Chrome. INTERLEAVE_SHOT=prefix also writes
// screenshots. It skips where node or Chrome is missing.
func TestThinkingInterleavesWithCards(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	call := func(id, name, args string) map[string]any {
		return map[string]any{"id": id, fnKey: map[string]any{fieldName: name, ilArgs: args}}
	}
	result := func(id, name string) map[string]any {
		return map[string]any{kRole: stepTool, kCallID: id, fieldName: name, kText: `{"ok": true}`}
	}
	step := func(think string, calls ...any) map[string]any {
		return map[string]any{kRole: roleAssistant, kText: "", ilThink: think, kCalls: calls}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "i1", Title: "Live order", Live: true, StartedAt: 900, Messages: []map[string]any{}})
	e.gw.AddSession(&gatewaytest.Session{ID: "i2", Title: "Stored order", Live: true, StartedAt: 901,
		Messages: []map[string]any{
			{kRole: roleUser, kText: "go"},
			step("First thought.", call("a1", "agent_peers", `{}`)),
			result("a1", "agent_peers"),
			step("Second thought.", call("a2", "agent_set_name", `{"name":"rezero-theories"}`)),
			result("a2", "agent_set_name"),
			step("", call("a3", "agent_peers", `{}`)),
			result("a3", "agent_peers"),
			step("Third thought.", call("a4", toolTerminal, `{"command":"ls"}`)),
			result("a4", toolTerminal),
			{kRole: roleAssistant, kText: ilDone},
		}})
	play := func() {
		tool := func(id, name string) {
			e.gw.Publish("i1", "tool.start", map[string]any{"tool_id": id, fieldName: name, "args": map[string]any{}})
			e.gw.Publish("i1", "tool.complete", map[string]any{"tool_id": id, fieldName: name, "result": `{"ok": true}`})
		}
		e.gw.Publish("i1", "message.start", map[string]any{})
		e.gw.Publish("i1", "reasoning.delta", map[string]any{kText: "First "})
		e.gw.Publish("i1", "reasoning.delta", map[string]any{kText: "thought."})
		tool("l1", "agent_peers")
		e.gw.Publish("i1", "reasoning.delta", map[string]any{kText: "Second thought."})
		tool("l2", "agent_set_name")
		tool("l3", "agent_peers")
		e.gw.Publish("i1", "reasoning.delta", map[string]any{kText: "Third thought."})
		e.gw.Publish("i1", "message.delta", map[string]any{kText: ilDone})
		e.gw.Publish("i1", "message.complete", map[string]any{keyStatus: ilComplete, kText: ilDone})
	}
	poke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		play()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(poke.Close)
	prof, err := os.MkdirTemp("", "interleave-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	cmd := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "interleave", "run.js"), chrome, e.url, prof, poke.URL)
	cmd.Env = append(os.Environ(), "SHOT_PREFIX="+os.Getenv("INTERLEAVE_SHOT"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
