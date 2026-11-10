package allowlist_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
)

func TestCheck(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	inside := filepath.Join(root, "a", "b")
	sibling := filepath.Join(base, "rootish")
	for _, d := range []string{inside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(sibling, escape); err != nil {
		t.Fatal(err)
	}
	l, err := allowlist.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		dir  string
		want error
	}{
		{root, nil},
		{inside, nil},
		{filepath.Join(inside, "..", ".."), nil},
		{sibling, allowlist.ErrOutside},
		{escape, allowlist.ErrOutside},
		{filepath.Join(root, "..", "rootish"), allowlist.ErrOutside},
		{"/", allowlist.ErrOutside},
		{"relative", allowlist.ErrNotAbsolute},
	}
	for _, tc := range cases {
		_, err := l.Check(tc.dir)
		if tc.want == nil && err != nil {
			t.Errorf("Check(%q) = %v, want nil", tc.dir, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("Check(%q) = %v, want %v", tc.dir, err, tc.want)
		}
	}
	if _, err := l.Check(filepath.Join(root, "missing")); err == nil {
		t.Error("missing dir accepted")
	}
}

func TestNewRejectsBadRoots(t *testing.T) {
	t.Parallel()
	if _, err := allowlist.New([]string{"rel"}); !errors.Is(err, allowlist.ErrNotAbsolute) {
		t.Errorf("relative root: %v", err)
	}
	if _, err := allowlist.New([]string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("missing root accepted")
	}
}
