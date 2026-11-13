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

// A scroll the reader did not make with a wheel, touch, pointer or key
// (find in page, focus on a link, a jump to an anchor) is not undone while
// the log grows; testdata/findscroll/run.js drives the real page in
// headless Chrome. FINDSCROLL_SHOT=prefix also writes screenshots. It skips
// where node or Chrome is missing.
func TestScrollNotFromTheReaderStays(t *testing.T) {
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
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Find scroll", Live: true, StartedAt: 900, Messages: ms})
	prof, err := os.MkdirTemp("", "findscroll-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	cmd := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "findscroll", "run.js"), chrome, e.url, prof)
	cmd.Env = append(os.Environ(), "SHOT_PREFIX="+os.Getenv("FINDSCROLL_SHOT"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
