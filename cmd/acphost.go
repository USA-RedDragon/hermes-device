package cmd

import "github.com/spf13/cobra"

func newACPHostCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "acp-host",
		Short: "In the Hermes pod: run one hermes acp per hub connection",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_ = b
			return ErrNotImplemented
		},
	}
}
