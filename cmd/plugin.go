package cmd

import (
	"fmt"

	"github.com/USA-RedDragon/hermes-device/internal/plugininstall"
	"github.com/USA-RedDragon/hermes-device/plugin"
	"github.com/spf13/cobra"
)

// installCommand is the name of the install subcommands.
const installCommand = "install"

func newPluginCommand(b *base) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage the embedded Hermes device plugin",
	}
	var dest string
	install := &cobra.Command{
		Use:   installCommand,
		Short: "Write the embedded device plugin into a Hermes plugins directory",
		Long: "Writes the device plugin files and a VERSION file into --dest, replacing an older copy. " +
			"Run it before Hermes starts, for example from an init container, then enable the plugin with hermes plugins enable device.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := plugininstall.Install(plugin.Files(), dest, b.version); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Installed the device plugin %s in %s\n", b.version, dest)
			return err
		},
	}
	install.Flags().StringVar(&dest, "dest", "", "Plugin directory to write, for example /opt/data/hermes/plugins/device")
	_ = install.MarkFlagRequired("dest")
	cmd.AddCommand(install)
	return cmd
}
