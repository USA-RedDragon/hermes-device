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

// perfLongChat is a long chat for the performance budget: 600 questions,
// 600 answers with a list and a code block, and a tool call with its result
// after every other answer (1,500 rows, 300 tool cards).
func perfLongChat(id string) []map[string]any {
	words := strings.Fields("the quick brown fox jumps over the lazy dog while a build runs tests and deploys a service")
	para := func(seed, n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(words[(seed*7+i*3)%len(words)])
			b.WriteByte(' ')
		}
		return b.String()
	}
	rows := make([]map[string]any, 0, 1500)
	for i := range 600 {
		rows = append(rows, map[string]any{kRole: roleUser, kText: fmt.Sprintf("%s question %d: %s", id, i, para(i, 12))})
		a := map[string]any{kRole: roleAssistant, kText: fmt.Sprintf("**%s answer %d.** %s\n\n- one %s\n- two %s\n\n```go\nfunc f%d() int { return %d }\n```\n", id, i, para(i, 60), para(i+1, 5), para(i+2, 5), i, i)}
		rows = append(rows, a)
		if i%2 == 0 {
			call := fmt.Sprintf("%s-c%d", id, i)
			a[kCalls] = []any{map[string]any{"id": call, fnKey: map[string]any{fieldName: toolTerminal, ilArgs: `{"command":"ls -la"}`}}}
			rows = append(rows, map[string]any{kRole: stepTool, kCallID: call, fieldName: toolTerminal,
				kText: `{"output": "` + para(i, 80+(i%7)*40) + `", "exit_code": 0, "error": null}`})
		}
	}
	return rows
}

// Opening and switching chats stays within its budget in headless Chrome:
// testdata/perfbudget/run.js times the first open of long chats and the
// switch back to them, against a calibration timed in the same page, and
// checks that the page holds a window of each long chat and shows a chat
// it switched back to from its own copy. PERF_REPORT=1 prints the numbers.
// It skips where node or Chrome is missing.
func TestOpenChatBudgetChrome(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	for i, id := range []string{"p0", "p1", "p2", "p3"} {
		e.gw.AddSession(&gatewaytest.Session{ID: id, Title: "Long " + id, Live: true, StartedAt: float64(1000 + i), Messages: perfLongChat(id)})
	}
	short := make([]map[string]any, 0, 20)
	for i := range 10 {
		short = append(short, map[string]any{kRole: roleUser, kText: fmt.Sprintf("short question %d", i)},
			map[string]any{kRole: roleAssistant, kText: fmt.Sprintf("short answer %d", i)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "ps", Title: "Short", Live: true, StartedAt: 900, Messages: short})
	prof, err := os.MkdirTemp("", "perfbudget-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	cmd := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "perfbudget", "run.js"), chrome, e.url, prof)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(out) > 0 {
		t.Logf("%s", out)
	}
}
