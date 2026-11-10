package main

import (
	"log/slog"
	"os"

	"github.com/USA-RedDragon/hermes-device/cmd"
	"github.com/USA-RedDragon/hermes-device/internal/browser/host"
)

// https://goreleaser.com/cookbooks/using-main.version/
//
//nolint:gochecknoglobals
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if manifest, extensionID, ok := host.Invocation(os.Args); ok {
		if err := cmd.RunNativeHost(manifest, extensionID); err != nil {
			slog.Error("Native messaging host failed.", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	rootCmd := cmd.NewCommand(version, commit)
	if err := rootCmd.Execute(); err != nil {
		slog.Error("Encountered an error.", "error", err.Error())
		os.Exit(1)
	}
}
