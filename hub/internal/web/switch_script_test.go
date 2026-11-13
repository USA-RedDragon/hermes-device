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

// Switching chats on a phone shows the chosen chat's title and a loading
// state, never the New chat empty state or the Jump to latest pill, and a
// tap, a long press or a tap on the message box never shows the pill;
// testdata/switch/run.js drives the real page in headless Chrome. It skips
// where node or Chrome is missing.
func TestSwitchingChatsShowsNoPillNoEmptyState(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	ms := make([]map[string]any, 0, 80)
	for i := range 40 {
		ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("question %d", i)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("Answer %d fills the log. ", i), 8)})
	}
	big := make([]map[string]any, 0, 1800)
	for i := range 900 {
		big = append(big, map[string]any{kRole: roleUser, kText: fmt.Sprintf("big question %d", i)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("Big answer %d. ", i), 30)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Long", Live: true, StartedAt: 900, Messages: ms})
	e.gw.AddSession(&gatewaytest.Session{ID: "s8", Title: titleSecond, Live: true, StartedAt: 800, Messages: ms[:4]})
	e.gw.AddSession(&gatewaytest.Session{ID: "bigchat", Title: "Big chat", Live: true, StartedAt: 700, Messages: big})
	prof, err := os.MkdirTemp("", "switch-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "switch", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
