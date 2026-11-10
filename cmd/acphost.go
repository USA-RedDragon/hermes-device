package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/internal/acphost"
	"github.com/spf13/cobra"
)

// ErrNoHermesHome is returned when acp-host cannot find the bindings file.
var ErrNoHermesHome = errors.New("set HERMES_HOME or acp-host.bindings-file")

func newACPHostCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "acp-host",
		Short: "In the Hermes pod: run one hermes acp per hub connection",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			if err := cfg.ForACPHost(); err != nil {
				return err
			}
			keyText := cfg.ACPHost.HubPublicKey
			if cfg.ACPHost.HubPublicKeyFile != "" {
				data, err := os.ReadFile(cfg.ACPHost.HubPublicKeyFile)
				if err != nil {
					return fmt.Errorf("acp-host.hub-public-key-file: %w", err)
				}
				keyText = string(data)
			}
			keys, err := assertion.ParsePublicKeys(keyText)
			if err != nil {
				return fmt.Errorf("hub public keys: %w", err)
			}
			bindings := cfg.ACPHost.BindingsFile
			if bindings == "" {
				home := os.Getenv("HERMES_HOME")
				if home == "" {
					return ErrNoHermesHome
				}
				bindings = filepath.Join(home, "device", "bindings.json")
			}
			ln, err := net.Listen("tcp", cfg.ACPHost.Listen)
			if err != nil {
				return err
			}
			srv := acphost.New(acphost.Options{
				Verifier:         assertion.NewVerifier(keys),
				BackendID:        cfg.ACPHost.BackendID,
				Hermes:           cfg.ACPHost.Hermes,
				Placeholder:      cfg.ACPHost.Placeholder,
				BindingsFile:     bindings,
				MaxProcesses:     cfg.ACPHost.MaxProcesses,
				PingInterval:     cfg.PingInterval,
				GatewayHealthURL: cfg.ACPHost.GatewayHealthURL,
				Logger:           slog.Default(),
			})
			slog.Info("acp-host listening", "addr", ln.Addr().String(), "backend", cfg.ACPHost.BackendID,
				"hermes", cfg.ACPHost.Hermes, "max_processes", cfg.ACPHost.MaxProcesses, "bindings", bindings,
				"gateway_health", cfg.ACPHost.GatewayHealthURL)
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return srv.Serve(ctx, ln)
		},
	}
}
