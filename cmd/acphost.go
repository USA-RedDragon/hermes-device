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
	"github.com/USA-RedDragon/hermes-device/internal/claudeusage"
	"github.com/USA-RedDragon/hermes-device/internal/config"
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
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ACPHost.Listen)
			if err != nil {
				return err
			}
			var usage acphost.UsageReporter
			col := hermesUsage(cfg)
			if col != nil {
				usage = col
			}
			defer startUsage(ctx, col, nil)()
			srv := acphost.New(acphost.Options{
				Verifier:         assertion.NewVerifier(keys),
				BackendID:        cfg.ACPHost.BackendID,
				Hermes:           cfg.ACPHost.Hermes,
				Placeholder:      cfg.ACPHost.Placeholder,
				BindingsFile:     bindings,
				MaxProcesses:     cfg.ACPHost.MaxProcesses,
				PingInterval:     cfg.PingInterval,
				GatewayHealthURL: cfg.ACPHost.GatewayHealthURL,
				Usage:            usage,
				Logger:           slog.Default(),
			})
			slog.Info("acp-host listening", "addr", ln.Addr().String(), "backend", cfg.ACPHost.BackendID,
				"hermes", cfg.ACPHost.Hermes, "max_processes", cfg.ACPHost.MaxProcesses, "bindings", bindings,
				"gateway_health", cfg.ACPHost.GatewayHealthURL, "claude_usage", usage != nil)
			return srv.Serve(ctx, ln)
		},
	}
}

// hermesUsage returns the collector of the Hermes pod's Claude Code
// usage: the claude-accounts slots, labeled from the plugin's settings,
// then usage.claude-dirs. Nil when usage.enabled is false.
func hermesUsage(cfg *config.Config) *claudeusage.Collector {
	if cfg.Usage.Enabled != nil && !*cfg.Usage.Enabled {
		return nil
	}
	hermesHome := os.Getenv("HERMES_HOME")
	root := cfg.Usage.HermesAccountsDir
	if root == "" {
		root = os.Getenv("CLAUDE_ACCOUNTS_ROOT")
	}
	if root == "" && hermesHome != "" {
		root = filepath.Join(hermesHome, "claude")
	}
	var sources []claudeusage.Source
	if root != "" {
		var slots []claudeusage.Slot
		if hermesHome != "" {
			slots = claudeusage.HermesSlots(filepath.Join(hermesHome, "config.yaml"))
		}
		sources = claudeusage.HermesSources(root, slots)
	}
	// Only the slots and usage.claude-dirs: ~/.claude may belong to a
	// computer whose own run daemon already counts it.
	for _, d := range cfg.Usage.ClaudeDirs {
		sources = append(sources, claudeusage.Source{Dir: d})
	}
	state := cfg.Usage.StateFile
	if state == "" && hermesHome != "" {
		state = filepath.Join(hermesHome, "device", "claude-usage.gob")
	}
	slog.Info("Counting Claude Code usage", "sources", len(sources))
	c := claudeusage.New(claudeusage.Options{Sources: sources, StateFile: state})
	c.Load()
	return c
}
