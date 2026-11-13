package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// agent.js reads the agent's health check, log lines and update status into
// what Settings, the update banner and the chat's quick panel show;
// testdata/agent/run.js checks its pure helpers.
func TestAgentScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "agent", "run.js"), filepath.Join("static", "agent.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
