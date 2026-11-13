package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/USA-RedDragon/talaria/internal/acpbridge"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/internal/tokenstore"
	"github.com/spf13/cobra"
)

func newACPCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "acp",
		Short: "Bridge an editor's ACP over stdio to the server",
		Long: "acp speaks the Agent Client Protocol with an editor on stdin and stdout and relays it to the " +
			"server's Hermes through the Talaria hub. Point the editor's agent command at `talaria acp`. " +
			"Logs go to stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			if err := cfg.ForHub(); err != nil {
				return err
			}
			store, err := acpTokenStore(cfg)
			if err != nil {
				return err
			}
			rec, err := store.Load(cfg.Hub.URL)
			if err != nil {
				return err
			}
			br := acpbridge.New(acpbridge.Options{
				Hub:          hubapi.New(cfg.Hub.URL, rec.Token, b.userAgent()),
				In:           os.Stdin,
				Out:          os.Stdout,
				MinBackoff:   cfg.Reconnect.MinBackoff,
				MaxBackoff:   cfg.Reconnect.MaxBackoff,
				PingInterval: cfg.PingInterval,
			})
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			err = br.Run(ctx)
			if errors.Is(err, hubapi.ErrUnauthorized) || errors.Is(err, acpbridge.ErrUnsupportedVersion) {
				fmt.Fprintln(os.Stderr, "talaria acp: "+err.Error())
			}
			return err
		},
	}
}

func acpTokenStore(cfg *config.Config) (*tokenstore.Store, error) {
	stateDir := cfg.Store.StateDir
	if stateDir == "" {
		dir, err := tokenstore.DefaultStateDir()
		if err != nil {
			return nil, err
		}
		stateDir = dir
	}
	file := cfg.Store.TokenFile
	if file == "" {
		file = filepath.Join(stateDir, "token")
	}
	return tokenstore.New(cfg.Store.Keyring, file), nil
}
