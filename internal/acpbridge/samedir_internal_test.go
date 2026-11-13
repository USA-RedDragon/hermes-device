package acpbridge

import (
	"os"
	"path/filepath"
	"testing"
)

// A binding stored with a symlink in its path (on macOS every temporary
// directory is under /var, a link to /private/var) still matches the
// editor's folder.
func TestSameDirResolvesBothSides(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	for _, tc := range []struct {
		bound, cwd string
		want       bool
	}{
		{dir, dir, true},
		{dir, link, true},
		{link, dir, true},
		{link, link, true},
		{dir, other, false},
		{link, other, false},
		{"", dir, false},
		{filepath.Join(dir, "missing"), dir, false},
	} {
		if got := sameDir(tc.bound, tc.cwd); got != tc.want {
			t.Errorf("sameDir(%q, %q) = %v, want %v", tc.bound, tc.cwd, got, tc.want)
		}
	}
}
