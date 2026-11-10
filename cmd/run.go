package cmd

import "github.com/spf13/cobra"

func newRunCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the device daemon: the exec leg and the chat splice",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_ = b
			return ErrNotImplemented
		},
	}
}
