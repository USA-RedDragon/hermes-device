package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/attest"
	"github.com/USA-RedDragon/hermes-device/internal/setup"
	"github.com/spf13/cobra"
)

func newUpdateCommand(b *base) *cobra.Command {
	var force, noRestart, check, skipAttestation bool
	c := &cobra.Command{
		Use:   "update [version]",
		Short: "Install the latest release (or the given one) for this OS and architecture, and restart the service",
		Long: "update downloads the release archive for this OS and architecture from GitHub, checks it against the " +
			"release's checksum file, verifies the archive's GitHub artifact attestation (Sigstore-signed build provenance " +
			"from this repository's release workflow at the release tag), checks that the new binary runs and reports the " +
			"release's version, replaces the installed binary (~/" + setup.BinDir + "/talaria when it exists, else this " +
			"binary) and restarts the service when it runs. It refuses a downgrade, and replacing a development build, " +
			"without --force. It refuses a release without a valid attestation, which includes every release before v" +
			setup.FirstAttestedVersion + ", without --insecure-skip-attestation.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sys, err := b.system()
			if err != nil {
				return err
			}
			sys.Out = cmd.OutOrStdout()
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			target, version, err := b.updateTarget(cmd, sys)
			if err != nil {
				return err
			}
			o := &setup.UpdateOptions{
				Target: target, Version: version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOARM: goarm(),
				Force: force, Restart: !noRestart, Releases: b.releases(),
				Check: check, SkipAttestation: skipAttestation, Attestations: b.attestations(),
			}
			if len(args) == 1 {
				o.Tag = args[0]
			}
			o.Releases.UserAgent = b.userAgent()
			tag, err := sys.Update(ctx, o)
			if err == nil && check {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s passed the checks. Nothing was installed.\n", tag)
				return err
			}
			if errors.Is(err, setup.ErrUpToDate) {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is %s, the latest release.\n", target, tag)
				return err
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Installed %s at %s.\n", tag, target)
			return err
		},
	}
	c.Flags().BoolVar(&force, "force", false, "Install even when it is a downgrade, the same version, or replaces a development build")
	c.Flags().BoolVar(&noRestart, "no-restart", false, "Do not restart the service")
	c.Flags().BoolVar(&check, "check", false, "Download and verify the release (checksum, attestation, the binary runs) but install nothing")
	c.Flags().BoolVar(&skipAttestation, "insecure-skip-attestation", false,
		"UNSAFE: install without verifying the release's attestation, for emergencies and releases before v"+setup.FirstAttestedVersion)
	return c
}

// updateTarget is the binary update replaces and its version: the one
// setup installed when it exists, else this binary.
func (b *base) updateTarget(cmd *cobra.Command, sys *setup.System) (string, string, error) {
	installed := setup.InstalledPath(sys.Home)
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", "", err
	}
	if resolved, err := filepath.EvalSymlinks(installed); err == nil {
		installed = resolved
	}
	if _, err := os.Stat(installed); err != nil || installed == exe {
		return exe, b.version, nil //nolint:nilerr // no installed binary: update this one
	}
	v, err := sys.InstalledVersion(cmd.Context(), installed)
	if err != nil {
		return "", "", fmt.Errorf("read the version of %s: %w", installed, err)
	}
	return installed, v, nil
}

// goarm is the ARM version this binary was built for, as the release
// archives name it.
func goarm() string {
	if runtime.GOARCH != "arm" {
		return ""
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" && s.Value != "" {
				return s.Value[:1]
			}
		}
	}
	return "6"
}

// attestations verifies release archives against this repository's
// release workflow, with the Sigstore public-good trusted root.
func (b *base) attestations() setup.AttestationVerifier {
	v := &attest.Verifier{
		APIURL:    attest.DefaultAPIURL,
		Client:    &http.Client{Timeout: 2 * time.Minute},
		UserAgent: b.userAgent(),
	}
	return func(ctx context.Context, tag, name string, digest []byte) (string, error) {
		p := attest.ReleasePolicy(tag)
		p.Name = name
		res, err := v.Verify(ctx, digest, p)
		if err != nil {
			return "", err
		}
		return res.SAN + " (commit " + res.Commit + ")", nil
	}
}
