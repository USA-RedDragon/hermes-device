package setup

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUpToDate is returned when the binary already has the release.
var ErrUpToDate = errors.New("already up to date")

// ErrAttestation is returned when the release archive has no attestation
// that passes verification.
var ErrAttestation = errors.New("the release archive's attestation did not verify")

// FirstAttestedVersion is the first release whose files have GitHub
// artifact attestations. Older releases cannot be verified.
const FirstAttestedVersion = "0.13.0"

// AttestationVerifier checks that an attestation of release tag's workflow
// covers the file name with SHA-256 digest, and returns who signed it.
type AttestationVerifier func(ctx context.Context, tag, name string, digest []byte) (string, error)

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
	// Attestations verifies the release archive before it is installed.
	Attestations AttestationVerifier
	// SkipAttestation installs without verifying the attestation. Unsafe.
	SkipAttestation bool
	// Check downloads and verifies the release but installs nothing.
	Check bool
	// Preflight, when set, are arguments that the new binary must run
	// with successfully before it replaces Target, for example a check
	// that it can load this computer's config.
	Preflight []string
}

// Update replaces o.Target with the binary of a release after checking
// the release's checksum, the archive's attestation, and that the new
// binary runs and reports the release's version. It returns the tag it
// installed. With o.Check it stops after the checks and installs nothing.
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
	if o.Check && o.SkipAttestation {
		return "", errors.New("--check with --insecure-skip-attestation would check nothing that matters; leave one out")
	}
	want := strings.TrimPrefix(tag, "v")
	if Canonical(want) == "" {
		return "", fmt.Errorf("%q is not a release version", o.Tag)
	}
	if !o.Force && !o.Check {
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
	archive, digest, err := o.Releases.FetchArchive(ctx, tag, o.GOOS, o.GOARCH, o.GOARM)
	if err != nil {
		return "", err
	}
	if err := s.verifyAttestation(ctx, o, tag, digest); err != nil {
		return "", err
	}
	data, err := ExtractBinary(archive)
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
	if err := s.preflight(ctx, tmp.Name(), o.Preflight); err != nil {
		return "", err
	}
	if o.Check {
		return tag, nil
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

// verifyAttestation checks the attestation of the archive with SHA-256
// digest, which is the archive Update installs from: it is held in
// memory, so it cannot change between the check and the install.
func (s *System) verifyAttestation(ctx context.Context, o *UpdateOptions, tag string, digest []byte) error {
	name := ArchiveName(strings.TrimPrefix(tag, "v"), o.GOOS, o.GOARCH, o.GOARM)
	if o.SkipAttestation {
		s.printf("WARNING: not verifying the attestation of %s (--insecure-skip-attestation). Only the checksum file, from the same place, vouches for it.\n", name)
		return nil
	}
	if o.Attestations == nil {
		return fmt.Errorf("%w: no attestation verifier", ErrAttestation)
	}
	signer, err := o.Attestations(ctx, tag, name, digest)
	if err == nil {
		s.printf("Verified the attestation of %s (sha256:%s), signed by %s.\n", name, hex.EncodeToString(digest), signer)
		return nil
	}
	if c, ok := Compare(strings.TrimPrefix(tag, "v"), FirstAttestedVersion); ok && c < 0 {
		return fmt.Errorf("%w: %w. %s is older than v%s, the first release with attestations; releases before it have none and "+
			"cannot be verified. Pass --insecure-skip-attestation to install it anyway (unsafe)", ErrAttestation, err, tag, FirstAttestedVersion)
	}
	return fmt.Errorf("%w, so %s was not installed. The file may have been changed after the release workflow built it. "+
		"Pass --insecure-skip-attestation only if you know why it fails (unsafe): %w", ErrAttestation, name, err)
}

// preflight runs the new binary at path with args, when there are any.
func (s *System) preflight(ctx context.Context, path string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if out, err := s.Run.Output(ctx, nil, path, args...); err != nil {
		return fmt.Errorf("the downloaded binary failed %q here, so it was not installed: %w: %s", args, err, bytes.TrimSpace(out))
	}
	return nil
}
