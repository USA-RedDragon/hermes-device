package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The quick panel and Settings show tools, skills and connections in
// words, never a snake_case or dotted id, and an id Talaria has no words
// for is humanized; testdata/ids/run.js drives the real page in headless
// Chrome. It skips where node or Chrome is missing.
func TestNoIdsInVisibleText(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setupAgent(t)
	e.pc.Toolsets = append(e.pc.Toolsets,
		map[string]any{"name": "brand_new_set", "label": "", "description": "calls new_tool and other_tool", "enabled": true, "configured": true},
		map[string]any{"name": "code_execution", "label": "Code Execution", "description": "execute_code", "enabled": true, "configured": true})
	e.pc.Skills = append(e.pc.Skills, map[string]any{"name": "weather_brief_daily", "description": "Short forecasts", "enabled": true, "provenance": "hub"})
	e.pc.Servers = append(e.pc.Servers, map[string]any{"name": "home_assistant", "transport": "http", "url": "https://ha.example/mcp", "enabled": true, "source": "config"})
	prof, err := os.MkdirTemp("", "ids-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "ids", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
