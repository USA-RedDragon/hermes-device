// Package allowlist decides whether a directory is inside one of the
// device's allowed root directories.
package allowlist

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrOutside is returned for a directory outside every allowed root.
var ErrOutside = errors.New("outside the allowed roots")

// ErrNotAbsolute is returned for a relative directory.
var ErrNotAbsolute = errors.New("not an absolute path")

// List holds the allowed roots, resolved through symlinks.
type List struct {
	roots []string
}

// New resolves each root. A root that does not exist is an error, so a typo
// in the config fails at startup instead of refusing everything later.
func New(roots []string) (*List, error) {
	l := &List{}
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("root %q: %w", r, ErrNotAbsolute)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(r))
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", r, err)
		}
		l.roots = append(l.roots, resolved)
	}
	return l, nil
}

// Roots returns the resolved roots.
func (l *List) Roots() []string {
	return append([]string(nil), l.roots...)
}

// Check resolves dir through symlinks and returns the resolved path when it
// is a root or below one.
func (l *List) Check(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q: %w", dir, ErrNotAbsolute)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("%q: %w", dir, err)
	}
	for _, root := range l.roots {
		if within(root, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%q: %w", dir, ErrOutside)
}

func within(root, path string) bool {
	if runtime.GOOS == "windows" {
		// Windows paths ignore case.
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}
