package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

const (
	longToken = "AAAAAAAAAABBBBBBBBBBCCCCCCCCCCDDDDDDDDDDEEEEEEEEEEFFFFFFFFFFGGGGGGGGGGHHHHHHHHHHIIIIIIIIIIJJJJJJJJJJKKKKKKKKKKLLLLLLLLLL"
	longURL   = "https://example.com/a/very/long/path/with/no/spaces/at/all/" + longToken + "?query=" + longToken + "&other=" + longToken
	// The script of the approval Jacob saw overflow on his phone.
	evalScript = "const t=document.body.innerText;const out=[];for(const k of ['ムーブメント','キャリバー','Cal','材質','生産国','電池','価格','税']){let i=-1;while((i=t.indexOf(k,i+1))>=0&&out.length<30)out.push(t.slice(Math.max(0,i-60),i+160).replace(/\\n+/g,' | '))}return out;"
	evalNote   = "Run a read-only script on iei.jp in the Firefox on device framework-13 with the user's logins (it can read what the page shows and send GET requests to iei.jp, nothing else). Approving for the session (or always) lets the agent run read-only scripts on iei.jp again without asking. Script:\n"
)

// No card, notice, table or code block may widen the chat stream or the
// page, on a phone or a desktop. testdata/overflow/run.js renders every card
// type with long unbroken content in headless Chrome at 360, 412 and 1280
// px and checks the page and the stream do not scroll sideways.
// OVERFLOW_SHOT=prefix also writes screenshots. It skips where node or
// Chrome is missing.
func TestNoCardOverflowsTheStream(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	call := func(id, name, args string) map[string]any {
		return map[string]any{"id": id, "function": map[string]any{"name": name, "arguments": args}}
	}
	table := "| a | b | c | d |\n|---|---|---|---|\n| " + longToken + " | " + longURL + " | " + longToken + " | x |\n"
	code := "```go\nfunc main() { fmt." + longToken + "(\"" + longToken + longToken + "\") }\n```\n"
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Work", Live: true, StartedAt: 900,
		Prompts: []gateway.Prompt{
			{Kind: gateway.PromptApproval, PromptID: "p-shell", Choices: []string{"once", "session", "deny"},
				Command: optr("delete " + longToken + longToken + " && curl " + longURL), Description: optr("dangerous command " + longToken)},
			{Kind: gateway.PromptApproval, PromptID: "p-eval", Choices: []string{"once", "session", "always", "deny"},
				Command: optr("<browser_eval> (plugin approval rule)"), Description: optr(evalNote + evalScript)},
			{Kind: gateway.PromptApproval, PromptID: "p-nav", Choices: []string{"once", "session", "always", "deny"},
				Command: optr("<browser_navigate> (plugin approval rule)"), Description: optr("Open " + longURL + " in the Firefox on device " + longToken + " with the user's logins.")},
			{Kind: gateway.PromptApproval, PromptID: "p-gen", Description: optr(longToken + longToken)},
			{Kind: gateway.PromptClarify, PromptID: "p-ask", Question: optr("Which? " + longToken + longToken), Choices: []string{longToken, "b"}},
		},
		Messages: []map[string]any{
			{kRole: roleUser, kText: "hello " + longToken + longToken + " " + longURL},
			{kRole: roleAssistant, kText: "Long url " + longURL + "\n\nLong token " + longToken + longToken + "\n\n" + table + "\n" + code + "\n`" + longToken + longToken + "`\n\n- item " + longURL + "\n\n> quote " + longToken + longToken,
				"tool_calls": []any{
					call("c1", "terminal", `{"command":"echo `+longToken+longToken+` && curl `+longURL+`"}`),
					call("c2", "browser_eval", `{"script":"`+longToken+longToken+`"}`),
					call("c3", "read_file", `{"path":"/a/`+longToken+longToken+`/b.go"}`),
					call("c4", "patch", `{"path":"/a/`+longToken+`/b.go"}`),
					call("c5", "web_search", `{"query":"`+longToken+longToken+`"}`)}},
			{kRole: "tool", "tool_call_id": "c1", "name": "terminal", kText: longToken + longToken + "\n" + longURL},
			{kRole: "tool", "tool_call_id": "c2", "name": "browser_eval", kText: `{"value":"` + longToken + longToken + `"}`},
			{kRole: "tool", "tool_call_id": "c3", "name": "read_file", kText: `{"content":"` + longToken + longToken + `"}`},
			{kRole: "tool", "tool_call_id": "c4", "name": "patch", kText: `{"success":true,"diff":"--- a\n+++ b\n@@ -1 +1 @@\n-` + longToken + longToken + `\n+` + longToken + `x"}`},
			{kRole: "tool", "tool_call_id": "c5", "name": "web_search", kText: longURL + longToken},
			{kRole: roleAssistant, kText: "[CONTEXT SUMMARY]: " + longToken + longToken + " " + longURL, "archived": true},
			{kRole: roleAssistant, kText: "```page\n<!doctype html><title>" + longToken + longToken + "</title><p>x</p>\n```\nafter"},
			{kRole: roleUser, kText: "[ASYNC DELEGATION TASK FAILED — " + longToken + "]\nTask: " + longToken + longToken + "\nStatus: failed   Duration: 3.0s\nError: " + longToken + longToken + " " + longURL + "\n"},
			{kRole: roleUser, kText: "[Agent message am_0123456789abcdef from " + longToken + "]\n" + longToken + longToken + " " + longURL},
			{kRole: roleUser, kText: "[IMPORTANT: Background process proc_1 `" + longToken + longToken + "` (exit code 1).\nlog " + longToken + longToken},
			{kRole: roleAssistant, kText: "done"},
		},
		Subagents: []gateway.SubagentRow{{SubagentID: "sa-0-a", Goal: longToken + longToken + " " + longURL, Status: optr(statusRunning), AcceptingSteer: optr(true)}},
	})
	prof, err := os.MkdirTemp("", "overflow-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "overflow", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.TrimSpace(string(out)))
	}
}
