package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

func TestUpdatePathHint(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home", "alice")
	dir := filepath.Join(home, setup.BinDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := "/usr/bin"
	sys := &setup.System{Home: home, Interactive: true, Getenv: func(k string) string {
		if k == "PATH" {
			return path
		}
		return ""
	}}
	bin := filepath.Join(dir, setup.BinaryName)
	want := `~/.talaria/bin is not on PATH, so the talaria and tal commands are "not found" in a terminal. Run ~/.talaria/bin/talaria setup to add it.` + "\n"
	if got := pathHint(sys, bin); got != want {
		t.Errorf("pathHint = %q", got)
	}
	if got := pathHint(sys, "/opt/talaria/talaria"); got != "" {
		t.Errorf("hint for a binary setup did not install: %q", got)
	}
	sys.Interactive = false
	if got := pathHint(sys, bin); got != "" {
		t.Errorf("hint without a terminal: %q", got)
	}
	sys.Interactive, path = true, "/usr/bin:"+dir
	if got := pathHint(sys, bin); got != "" {
		t.Errorf("hint with the directory on PATH: %q", got)
	}
}
