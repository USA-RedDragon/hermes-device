package cmd

import "github.com/spf13/cobra"

func newACPCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "acp",
		Short: "Bridge an editor's ACP over stdio to the server",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_ = b
			return ErrNotImplemented
		},
	}
}
