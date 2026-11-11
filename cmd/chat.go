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
	var allowMismatch, status, refresh bool
	c := &cobra.Command{
		Use:   "chat [-- hermes-args...]",
		Short: "Open the local Hermes TUI on the server, in this directory",
		Long: "chat starts the local Hermes TUI (hermes --tui) in the current directory and points it at " +
			"the running talaria run daemon, which carries it to the server's Hermes. On Linux it runs the exact build " +
			"the server runs, which run fetches from the server's container image and verifies (chat.managed); otherwise, " +
			"or when the server reports no image, it runs chat.hermes of the same release. Arguments after -- go to Hermes. " +
			"Hermes' -c/--continue and -r/--resume flags are looked up on the server: -c alone continues the most recent " +
			"session bound to this device. --status shows the managed builds and --refresh fetches the server's build again.",
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
			builds := hermesBuilds(cfg, b.userAgent())
			if builds != nil {
				builds.Progress = progress(cmd.ErrOrStderr())
			}
			switch {
			case status:
				return printStatus(cmd.OutOrStdout(), cfg, builds, st)
			case refresh:
				return refreshBuild(cmd.Context(), cmd.OutOrStdout(), builds, st)
			}
			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("current directory: %w", err)
			}
			code, err := chat.Run(cmd.Context(), chat.Params{
				Hermes:               cfg.Chat.Hermes,
				Builds:               builds,
				Image:                st.HermesImage,
				BaseImage:            st.HermesBaseImage,
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
	c.Flags().BoolVar(&status, "status", false, "Show the server's Hermes and the managed builds on this device, then exit")
	c.Flags().BoolVar(&refresh, "refresh", false,
		"Fetch and verify the server's Hermes build again, remove old builds, then exit")
	c.MarkFlagsMutuallyExclusive("status", "refresh")
	return c
}
