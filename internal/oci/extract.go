package oci

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
)

// Filter selects layer entries by path. Paths are slash-separated and
// relative to the image root, such as opt/app/bin/app. Patterns use
// path.Match syntax and match a path when they match its leading
// components.
type Filter struct {
	// Include lists the trees to extract. Empty extracts everything.
	Include []string
	// Exclude lists trees inside them to leave out.
	Exclude []string
}

// Keep reports whether the entry at name is extracted.
func (f Filter) Keep(name string) bool {
	if len(f.Include) > 0 && !matchAny(f.Include, name) {
		return false
	}
	return !matchAny(f.Exclude, name)
}

// touches reports whether a whiteout of name can remove a kept entry:
// name is kept, or an included tree lies inside it.
func (f Filter) touches(name string) bool {
	if f.Keep(name) {
		return true
	}
	for _, inc := range f.Include {
		if strings.HasPrefix(inc+"/", name+"/") {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, name string) bool {
	parts := strings.Split(name, "/")
	for _, p := range patterns {
		n := strings.Count(p, "/") + 1
		if n > len(parts) {
			continue
		}
		if ok, _ := path.Match(p, strings.Join(parts[:n], "/")); ok {
			return true
		}
	}
	return false
}

// Whiteout markers (OCI image spec, layer changesets).
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// extractor applies layers in order to root. os.Root refuses every path
// that resolves outside root, through ".." or through a symlink, so no
// entry can write outside it.
type extractor struct {
	root    *os.Root
	filter  Filter
	log     *slog.Logger
	budget  int64
	entries int
	// created holds the paths this layer wrote, and their parents, which
	// its whiteouts must not remove.
	created map[string]bool
}

// cleanName validates a layer path and returns it relative and clean.
func cleanName(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}
	n := strings.TrimLeft(name, "/")
	for p := range strings.SplitSeq(n, "/") {
		if p == ".." {
			return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
		}
	}
	n = path.Clean(n)
	if n == "." || n == "" {
		return "", nil
	}
	return n, nil
}

func (x *extractor) layer(r io.Reader) error {
	x.created = map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if x.entries++; x.entries > MaxEntries {
			return fmt.Errorf("more than %d entries: %w", MaxEntries, ErrTooLarge)
		}
		name, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if err := x.entry(tr, hdr, name); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
}

func (x *extractor) entry(tr *tar.Reader, hdr *tar.Header, name string) error {
	if ok, err := x.whiteout(name); ok {
		return err
	}
	if !x.filter.Keep(name) {
		return nil
	}
	switch hdr.Typeflag {
	case tar.TypeDir, tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
	default:
		return nil
	}
	if err := x.clear(name, hdr.Typeflag == tar.TypeDir); err != nil {
		return err
	}
	x.markCreated(name)
	mode := hdr.FileInfo().Mode().Perm()
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := x.root.Mkdir(name, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		return x.root.Chmod(name, mode|0o700)
	case tar.TypeSymlink:
		if strings.ContainsRune(hdr.Linkname, 0) || hdr.Linkname == "" {
			return fmt.Errorf("%w: link %q", ErrUnsafePath, hdr.Linkname)
		}
		return x.root.Symlink(hdr.Linkname, name)
	case tar.TypeLink:
		return x.hardLink(hdr.Linkname, name)
	}
	return x.file(tr, hdr, name, mode)
}

// whiteout applies name when it is a whiteout marker and reports whether
// it was one.
func (x *extractor) whiteout(name string) (bool, error) {
	dir, base := path.Dir(name), path.Base(name)
	if base == whiteoutOpaque {
		if dir == "." || !x.filter.touches(dir) {
			return true, nil
		}
		return true, x.clearDir(dir)
	}
	target, ok := strings.CutPrefix(base, whiteoutPrefix)
	if !ok {
		return false, nil
	}
	if target == "" || target == "." || target == ".." {
		return true, fmt.Errorf("%w: whiteout %q", ErrUnsafePath, name)
	}
	target = path.Join(dir, target)
	if !x.filter.touches(target) || x.created[target] {
		return true, nil
	}
	return true, removeAll(x.root, target)
}

// clear makes the parents of name and removes what a lower layer has at
// name, unless both are directories, which merge.
func (x *extractor) clear(name string, isDir bool) error {
	if dir := path.Dir(name); dir != "." {
		if err := x.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	fi, err := x.root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.IsDir() && isDir:
		return nil
	}
	return removeAll(x.root, name)
}

func (x *extractor) hardLink(linkname, name string) error {
	target, err := cleanName(linkname)
	if err != nil || target == "" {
		return fmt.Errorf("%w: hard link to %q", ErrUnsafePath, linkname)
	}
	if !x.filter.Keep(target) {
		x.log.Warn("Skipping a hard link to a file that the filter leaves out", "link", name, "target", target)
		return nil
	}
	return x.root.Link(target, name)
}

func (x *extractor) file(tr *tar.Reader, hdr *tar.Header, name string, mode fs.FileMode) error {
	if hdr.Size < 0 || hdr.Size > x.budget {
		return fmt.Errorf("unpacked size: %w", ErrTooLarge)
	}
	x.budget -= hdr.Size
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(tr, hdr.Size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != hdr.Size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return err
	}
	return x.root.Chtimes(name, hdr.ModTime, hdr.ModTime)
}

func (x *extractor) markCreated(name string) {
	for p := name; p != "." && !x.created[p]; p = path.Dir(p) {
		x.created[p] = true
	}
}

// clearDir removes what lower layers put in dir. It leaves a dir that is
// a symlink alone: this layer's entries name the link's target by other
// paths, which created would not match.
func (x *extractor) clearDir(dir string) error {
	if fi, err := x.root.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	d, err := x.root.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		p := path.Join(dir, n)
		if !x.created[p] {
			if err := removeAll(x.root, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeAll(root *os.Root, name string) error {
	err := root.RemoveAll(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
