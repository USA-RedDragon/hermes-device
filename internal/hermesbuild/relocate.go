package hermesbuild

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// relocate makes the tree in dir work from final, where it will live,
// instead of from /. Every absolute symlink becomes a relative one into
// the tree, and an absolute home in a pyvenv.cfg moves under final. The
// image links .venv/bin/python to /usr/local/bin/python3 and that to the
// bundled interpreter, and its venv's pyvenv.cfg names /usr/local/bin.
func relocate(dir, final string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return relinkAbsolute(p, rel)
		case d.Type().IsRegular() && d.Name() == "pyvenv.cfg":
			return rewritePyvenv(p, final)
		}
		return nil
	})
}

func relinkAbsolute(p, rel string) error {
	target, err := os.Readlink(p)
	if err != nil || !filepath.IsAbs(target) {
		return err
	}
	in := strings.TrimPrefix(filepath.Clean(target), "/")
	newTarget, err := filepath.Rel(filepath.Dir(rel), in)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	return os.Symlink(newTarget, p)
}

func rewritePyvenv(p, final string) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	changed := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "home" && filepath.IsAbs(strings.TrimSpace(val)) {
			line = "home = " + filepath.Join(final, strings.TrimSpace(val))
			changed = true
		}
		out.WriteString(line + "\n")
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if !changed {
		return nil
	}
	return os.WriteFile(p, out.Bytes(), 0o600)
}
