// Package plugininstall writes the embedded Hermes device plugin into a
// Hermes plugins directory.
package plugininstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// VersionFile is written last, next to plugin.yaml, and names the
// talaria build that wrote the tree.
const VersionFile = "VERSION"

// ErrNotPluginDir is returned when dest exists and does not look like a
// device plugin directory written by this installer, so it is not replaced.
var ErrNotPluginDir = errors.New("destination exists and is not a talaria plugin directory")

// Install writes every file of src into dest and then VERSION holding
// version. The tree is built in a sibling directory and swapped in with
// renames, so a reader sees the old tree, the new one or, for a moment,
// none, never a mix of the two.
func Install(src fs.FS, dest, version string) error {
	dest = filepath.Clean(dest)
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("destination %q must be an absolute path", dest)
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(dest); err == nil {
		if !fi.IsDir() {
			return ErrNotPluginDir
		}
		if _, err := os.Stat(filepath.Join(dest, "plugin.yaml")); err != nil {
			entries, _ := os.ReadDir(dest)
			if len(entries) > 0 {
				return ErrNotPluginDir
			}
		}
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := copyTree(src, staging); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(staging, VersionFile), []byte(strings.TrimSpace(version)+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return err
	}
	old := ""
	if _, err := os.Lstat(dest); err == nil {
		old = staging + ".old"
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		if old != "" {
			_ = os.Rename(old, dest)
		}
		return err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return syncDir(parent)
}

func copyTree(src fs.FS, dst string) error {
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return writeFileSync(target, data, 0o644)
	})
}

func writeFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
