package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCopySelf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	root := NewCommand("v0.0.0", "none", func(b *base) {
		b.executable = func() (string, error) { return src, nil }
	})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{copySelfCommand, dst})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "binary" {
		t.Fatalf("dst = %q, %v", got, err)
	}
	if fi, _ := os.Stat(dst); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("temporary file left behind: %v", entries)
	}
}

func TestCopySelfHiddenAndSkipsMigration(t *testing.T) {
	t.Parallel()
	root := NewCommand("v0.0.0", "none")
	c, _, err := root.Find([]string{copySelfCommand})
	if err != nil || !c.Hidden || !skipMigration(c) {
		t.Errorf("hidden=%v skip=%v err=%v", c.Hidden, skipMigration(c), err)
	}
}
