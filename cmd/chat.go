package cmd

import "github.com/spf13/cobra"

func newChatCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "chat",
		Short: "Open the local Hermes TUI on the server, in this directory",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_ = b
			return ErrNotImplemented
		},
	}
}
