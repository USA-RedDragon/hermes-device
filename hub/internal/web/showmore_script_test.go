package web_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// In the chat's quick panel, Show N more under Tools and Show all under
// Skills expand the list in place and keep the panel open, and a click
// outside still closes it; testdata/showmore/run.js drives the real page in
// headless Chrome at 1280 and 412 px. SHOWMORE_SHOT=dir also writes
// screenshots. It skips where node or Chrome is missing.
func TestQuickPanelShowMoreKeepsPanelOpen(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setupAgent(t)
	for i := range 21 {
		e.pc.Toolsets = append(e.pc.Toolsets, toolset("extra_tool_"+strconv.Itoa(i), fmt.Sprintf("Extra tool %d", i), "does a thing"))
	}
	for i := range 70 {
		e.pc.Skills = append(e.pc.Skills, map[string]any{fieldName: fmt.Sprintf("extra_skill_%d", i), keyDesc: "a skill", keyEnabled: true, "provenance": "hub"})
	}
	prof, err := os.MkdirTemp("", "showmore-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "showmore", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

const keyDesc = "description"
