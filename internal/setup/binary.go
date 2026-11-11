package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"
)

// BinDir is where setup installs the binary, under the home directory.
const BinDir = ".hermes-device/bin"

// BinaryName is the binary's file name.
const BinaryName = "hermes-device"

// ErrDowngrade is returned when the installed binary is newer than the one
// that would replace it.
var ErrDowngrade = errors.New("refusing to replace a newer hermes-device")

// ParseVersion returns the version that hermes-device --version printed
// ("hermes-device version 0.8.1 - abc1234").
func ParseVersion(out string) string {
	_, rest, ok := strings.Cut(strings.TrimSpace(out), " version ")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, " ")
	return v
}

// Canonical returns v as a semver string with a leading v, or "" when v is
// not a release version (a development build, for example).
func Canonical(v string) string {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return semver.Canonical(v)
}

// Compare compares two versions as semver; ok is false when either is not
// a release version.
func Compare(a, b string) (cmp int, ok bool) {
	ca, cb := Canonical(a), Canonical(b)
	if ca == "" || cb == "" {
		return 0, false
	}
	return semver.Compare(ca, cb), true
}

// InstalledVersion runs path --version.
func (s *System) InstalledVersion(ctx context.Context, path string) (string, error) {
	out, err := s.Run.Output(ctx, nil, path, "--version")
	if err != nil {
		return "", err
	}
	return ParseVersion(string(out)), nil
}

// InstallBinary copies the binary src (of version) to dst, unless dst is
// src or already holds the same bytes. It refuses to replace a newer
// release with an older one unless force is set. It reports whether it
// wrote dst.
func (s *System) InstallBinary(ctx context.Context, src, dst, version string, force bool) (bool, error) {
	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		return false, err
	}
	if fs1, err := os.Stat(src); err == nil {
		if fs2, err := os.Stat(dst); err == nil && os.SameFile(fs1, fs2) {
			return false, nil
		}
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	old, err := os.ReadFile(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	case bytes.Equal(old, data):
		return false, nil
	case !force:
		have, verr := s.InstalledVersion(ctx, dst)
		if c, ok := Compare(version, have); verr == nil && ok && c < 0 {
			return false, fmt.Errorf("%w: %s is %s and this binary is %s; run that one, or pass --force", ErrDowngrade, dst, have, version)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // a program directory
		return false, err
	}
	return true, writeFileAtomic(dst, data, 0o755)
}
