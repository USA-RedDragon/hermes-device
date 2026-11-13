package allowlist_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/allowlist"
)

func TestNewRejectsBadRoots(t *testing.T) {
	t.Parallel()
	if _, err := allowlist.New([]string{"rel"}); !errors.Is(err, allowlist.ErrNotAbsolute) {
		t.Errorf("relative root: %v", err)
	}
	if _, err := allowlist.New([]string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("missing root accepted")
	}
}

func TestCheckComparesResolvedPaths(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(base, "private", "work")
	other := filepath.Join(base, "private", "etc")
	for _, d := range []string{realDir, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "work")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(base, "etc")
	if err := os.Symlink(other, etc); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{"root by link": link, "root by real path": realDir} {
		l, err := allowlist.New([]string{root})
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{link, realDir} {
			if got, err := l.Check(dir); err != nil || got != realDir {
				t.Errorf("%s: Check(%s) = %q, %v", name, dir, got, err)
			}
		}
		for _, dir := range []string{etc, other} {
			if _, err := l.Check(dir); !errors.Is(err, allowlist.ErrOutside) {
				t.Errorf("%s: Check(%s) = %v, want ErrOutside", name, dir, err)
			}
		}
	}
}
