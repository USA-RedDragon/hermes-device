package cmd

import "github.com/spf13/cobra"

func newLoginCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Pair this device with hermes-hub and store its token",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_ = b
			return ErrNotImplemented
		},
	}
}
