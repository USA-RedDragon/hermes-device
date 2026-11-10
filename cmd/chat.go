package cmd

import (
	"fmt"
	"os"

	"github.com/USA-RedDragon/hermes-device/internal/chat"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
	"github.com/spf13/cobra"
)

func newChatCommand(b *base) *cobra.Command {
	var allowMismatch bool
	c := &cobra.Command{
		Use:   "chat [-- hermes-args...]",
		Short: "Open the local Hermes TUI on the server, in this directory",
		Long: "chat starts the local Hermes TUI (<chat.hermes> --tui) in the current directory and points it at " +
			"the running hermes-device run daemon, which carries it to the server's Hermes. Arguments after -- go to Hermes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			if err := cfg.ForHub(); err != nil {
				return err
			}
			stateDir, err := chat.StateDir(cfg)
			if err != nil {
				return err
			}
			rec, err := tokenstore.New(cfg.Store.Keyring, chat.TokenFile(cfg, stateDir)).Load(cfg.Hub.URL)
			if err != nil {
				return err
			}
			st, err := chat.ReadState(stateDir, cfg.Hub.URL)
			if err != nil {
				return err
			}
			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("current directory: %w", err)
			}
			code, err := chat.Run(cmd.Context(), chat.Params{
				Hermes:               cfg.Chat.Hermes,
				Args:                 args,
				Hub:                  hubapi.New(cfg.Hub.URL, rec.Token, b.userAgent()),
				Control:              &chat.Control{State: st},
				AllowVersionMismatch: allowMismatch,
				Stdin:                os.Stdin,
				Stdout:               os.Stdout,
				Stderr:               os.Stderr,
				Dir:                  dir,
			})
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code) //nolint:revive // the TUI's exit code is chat's
			}
			return nil
		},
	}
	c.Flags().BoolVar(&allowMismatch, "allow-version-mismatch", false,
		"Start the TUI even when the local Hermes is not the server's release")
	return c
}
