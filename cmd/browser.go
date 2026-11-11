package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	firefox "github.com/USA-RedDragon/hermes-device/extension/firefox"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/browser/host"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
	"github.com/spf13/cobra"
)

func newBrowserCommand(b *base) *cobra.Command {
	c := &cobra.Command{
		Use:   "browser",
		Short: "Firefox browser control (docs/browser.md)",
	}
	c.AddCommand(&cobra.Command{
		Use:     installCommand,
		Aliases: []string{"install-host"},
		Short:   "Register this binary as the native messaging host of the Hermes Firefox extension, for this user",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			sock, err := browserSocket(cfg)
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			path, err := installHostManifest(exe, home, sock)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, err = fmt.Fprintf(out, "Wrote %s\nThe extension reaches the run daemon at %s.\n", path, sock)
			if err == nil && !cfg.Browser.Enabled {
				_, err = fmt.Fprintln(out, "Set browser.enabled to true in the config and restart talaria run.")
			}
			return err
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Remove the native messaging host manifest that install wrote",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if err := removeHostManifest(home); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Removed the native messaging host from", host.ManifestDir(home))
			return err
		},
	})
	var out, extVersion string
	ext := &cobra.Command{
		Use:   "extension",
		Short: "Write the Hermes Firefox extension's files to a directory, to load as a temporary add-on",
		Long: "Write the Hermes Firefox extension's files to a directory, to load as a temporary add-on. " +
			"Each release also publishes the extension signed by Mozilla, which installs for good and updates itself " +
			"(docs/browser.md section 6.1).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := extVersion
			if !cmd.Flags().Changed("ext-version") && firefox.ValidVersion(b.version) {
				v = b.version
			}
			if err := firefox.Write(out, v); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"Wrote the extension to %s. In Firefox, open about:debugging#/runtime/this-firefox, choose "+
					"\"Load Temporary Add-on\" and pick %s.\n"+
					"To install it for good, with updates, install talaria-firefox-<version>.xpi from %s "+
					"(docs/browser.md section 6.1).\n",
				out, filepath.Join(out, "manifest.json"), firefox.ReleasesURL)
			return err
		},
	}
	ext.Flags().StringVarP(&out, "out", "o", "hermes-browser-extension", "Directory to write the extension to")
	ext.Flags().StringVar(&extVersion, "ext-version", "",
		"Version to put in manifest.json (default: this binary's release version, else the embedded one)")
	c.AddCommand(ext)
	return c
}

// browserSocket is the run daemon's browser socket path for cfg.
func browserSocket(cfg *config.Config) (string, error) {
	dir := cfg.Store.StateDir
	if dir == "" {
		d, err := tokenstore.DefaultStateDir()
		if err != nil {
			return "", fmt.Errorf("find the state directory: %w", err)
		}
		dir = d
	}
	return filepath.Join(dir, broker.SocketName), nil
}

// removeHostManifest removes what installHostManifest wrote under home.
func removeHostManifest(home string) error {
	dir := host.ManifestDir(home)
	for _, name := range []string{host.Name + ".json", host.SocketFileName, host.LegacyName + ".json", host.LegacyName + ".socket"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// installHostManifest registers the binary exe as the native messaging
// host for Firefox under home, with the daemon socket sock.
func installHostManifest(exe, home, sock string) (string, error) {
	exe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(host.NewManifest(exe), "", "  ")
	if err != nil {
		return "", err
	}
	dir := host.ManifestDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Firefox reads it; it holds no secret
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, host.SocketFileName), []byte(sock+"\n"), 0o644); err != nil { //nolint:gosec // as above
		return "", err
	}
	path := filepath.Join(dir, host.Name+".json")
	return path, os.WriteFile(path, append(data, '\n'), 0o644) //nolint:gosec // as above
}

// nativeHostSocket finds the run daemon's browser socket for the native
// host: TALARIA_BROWSER_SOCKET (or the legacy TALARIA_BROWSER_SOCKET), else the path install wrote next to
// the manifest, else the default state directory.
func nativeHostSocket(manifest string) (string, error) {
	if p := config.Getenv(broker.SocketEnv); p != "" {
		return p, nil
	}
	if p := host.SocketFromManifest(manifest); p != "" {
		return p, nil
	}
	dir, err := tokenstore.DefaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, broker.SocketName), nil
}

// RunNativeHost is what main runs when Firefox starts the binary as the
// extension's native messaging host. It speaks native messaging on stdin and
// stdout.
func RunNativeHost(manifest, extensionID string) error {
	sock, err := nativeHostSocket(manifest)
	if err != nil {
		return err
	}
	return host.Run(os.Stdin, os.Stdout, sock, extensionID)
}
