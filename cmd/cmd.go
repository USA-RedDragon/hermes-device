// Package cmd wires the hermes-device command line.
package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/spf13/cobra"
)

// ErrNotImplemented is returned by subcommands that are not built yet.
var ErrNotImplemented = errors.New("not implemented")

// ErrWindows is returned on Windows, which hermes-device does not support.
var ErrWindows = errors.New("hermes-device does not support Windows: device paths must be POSIX")

// NewCommand returns the root command.
func NewCommand(version, commit string) *cobra.Command {
	root := &cobra.Command{
		Use:   "hermes-device",
		Short: "Run a server Hermes agent's shell and file operations on this device",
		Long: "hermes-device pairs this device with hermes-hub (login), runs the agent's commands here (run), " +
			"opens the local Hermes TUI against the server (chat) and bridges editors over ACP (acp). " +
			"In the Hermes pod it installs the device plugin (plugin install) and hosts ACP processes (acp-host).",
		Version:       fmt.Sprintf("%s - %s", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if runtime.GOOS == "windows" {
				return ErrWindows
			}
			return nil
		},
	}
	b := &base{
		version: version,
		commit:  commit,
		loader:  config.NewLoader(root.PersistentFlags(), defaultConfigFiles()),
	}
	root.AddCommand(
		newLoginCommand(b),
		newRunCommand(b),
		newChatCommand(b),
		newACPCommand(b),
		newACPHostCommand(b),
		newPluginCommand(b),
	)
	return root
}

// base is what every subcommand shares.
type base struct {
	version string
	commit  string
	loader  *configulator.Configulator[config.Config]
}

// load reads the configuration and sets up logging to stderr. stdout stays
// free for subcommands that speak a protocol on it (acp).
func (b *base) load() (*config.Config, error) {
	cfg, err := b.loader.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return cfg, nil
}

func (b *base) userAgent() string {
	return "hermes-device/" + b.version
}

func defaultConfigFiles() []string {
	files := []string{"config.yaml"}
	if dir, err := os.UserConfigDir(); err == nil {
		files = append(files, filepath.Join(dir, "hermes-device", "config.yaml"))
	}
	return files
}
