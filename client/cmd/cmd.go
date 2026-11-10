// Package cmd wires the device client's command line.
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/USA-RedDragon/hermes-device/client/internal/client"
	"github.com/USA-RedDragon/hermes-device/client/internal/config"
	"github.com/spf13/cobra"
)

// NewCommand returns the root command.
func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hermes-device",
		Short:   "Run a Hermes agent's shell and file operations on this device",
		Version: fmt.Sprintf("%s - %s", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	loader := config.NewLoader(cmd.Flags())
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := loader.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return run(cmd, cfg)
	}
	return cmd
}

func run(cmd *cobra.Command, cfg *config.Config) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	slog.Info("Hermes device client", "version", cmd.Annotations["version"], "commit", cmd.Annotations["commit"])

	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err = c.Run(ctx)
	slog.Info("Shut down")
	return err
}
