package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// minichat.js draws a subagent's steps as a small chat with the chat's tool
// cards and groups (toolgroup.js): it follows new steps at the end, counts
// them in a pill when the reader scrolled up, and says whether the
// subagent read each message the user sent.
func TestMiniChatScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	files := []string{"times.js", "toolcard.js", "toolgroup.js", "minichat.js"}
	args := make([]string, 0, 1+len(files))
	args = append(args, filepath.Join("testdata", "minichat", "run.js"))
	for _, f := range files {
		args = append(args, filepath.Join("static", f))
	}
	out, err := exec.CommandContext(t.Context(), node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
