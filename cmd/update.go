package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/USA-RedDragon/hermes-device/internal/setup"
	"github.com/spf13/cobra"
)

func newUpdateCommand(b *base) *cobra.Command {
	var force, noRestart bool
	c := &cobra.Command{
		Use:   "update [version]",
		Short: "Install the latest release (or the given one) for this OS and architecture, and restart the service",
		Long: "update downloads the release archive for this OS and architecture from GitHub, checks it against the " +
			"release's checksum file, checks that the new binary runs and reports the release's version, replaces " +
			"the installed binary (~/" + setup.BinDir + "/hermes-device when it exists, else this binary) and restarts " +
			"the service when it runs. It refuses a downgrade, and replacing a development build, without --force.",
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
			}
			if len(args) == 1 {
				o.Tag = args[0]
			}
			o.Releases.UserAgent = b.userAgent()
			tag, err := sys.Update(ctx, o)
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
	return c
}

// updateTarget is the binary update replaces and its version: the one
// setup installed when it exists, else this binary.
func (b *base) updateTarget(cmd *cobra.Command, sys *setup.System) (string, string, error) {
	installed := filepath.Join(sys.Home, setup.BinDir, setup.BinaryName)
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
