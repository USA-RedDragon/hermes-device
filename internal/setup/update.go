package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUpToDate is returned when the binary already has the release.
var ErrUpToDate = errors.New("already up to date")

// ErrDevBuild is returned when a development build would be replaced
// without --force.
var ErrDevBuild = errors.New("this binary is a development build")

// UpdateOptions configure Update.
type UpdateOptions struct {
	// Target is the binary to replace; Version is its version.
	Target, Version string
	// Tag is the release to install; empty means the latest.
	Tag string
	// GOOS, GOARCH and GOARM pick the release archive.
	GOOS, GOARCH, GOARM string
	// Force allows a downgrade, a reinstall and replacing a development
	// build.
	Force bool
	// Restart restarts the service when it runs.
	Restart  bool
	Releases *Releases
}

// Update replaces o.Target with the binary of a release after checking
// the release's checksum and that the new binary runs and reports the
// release's version. It returns the tag it installed.
func (s *System) Update(ctx context.Context, o *UpdateOptions) (string, error) {
	tag := o.Tag
	if tag == "" {
		latest, err := o.Releases.LatestTag(ctx)
		if err != nil {
			return "", fmt.Errorf("find the latest release: %w", err)
		}
		tag = latest
	} else if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	want := strings.TrimPrefix(tag, "v")
	if Canonical(want) == "" {
		return "", fmt.Errorf("%q is not a release version", o.Tag)
	}
	if !o.Force {
		c, ok := Compare(want, o.Version)
		switch {
		case !ok:
			return "", fmt.Errorf("%w (version %q); pass --force to replace it with %s", ErrDevBuild, o.Version, tag)
		case c == 0:
			return tag, fmt.Errorf("%w: %s is %s", ErrUpToDate, o.Target, tag)
		case c < 0:
			return "", fmt.Errorf("%w: %s is %s, newer than %s; pass --force to install %s anyway", ErrDowngrade, o.Target, o.Version, tag, tag)
		}
	}
	data, err := o.Releases.FetchBinary(ctx, tag, o.GOOS, o.GOARCH, o.GOARM)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(o.Target)
	tmp, err := os.CreateTemp(dir, ".hermes-device.new-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil { //nolint:gosec // a program
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got, err := s.InstalledVersion(ctx, tmp.Name())
	if err != nil {
		return "", fmt.Errorf("the downloaded binary does not run here: %w", err)
	}
	if got != want {
		return "", fmt.Errorf("the downloaded binary reports version %q, want %q", got, want)
	}
	if err := os.Rename(tmp.Name(), o.Target); err != nil {
		return "", err
	}
	if !o.Restart {
		return tag, nil
	}
	svc, err := s.NewService()
	if err != nil || svc.Available(ctx) != nil {
		return tag, nil //nolint:nilerr // no service manager: nothing to restart
	}
	if _, err := os.Stat(svc.Path()); err != nil || !svc.Active(ctx) {
		return tag, nil //nolint:nilerr // no service installed or running
	}
	if err := svc.Restart(ctx); err != nil {
		return tag, fmt.Errorf("installed %s, but the restart failed: %w", tag, err)
	}
	s.printf("Restarted the %s.\n", svc.Name())
	return tag, nil
}
