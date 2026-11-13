//go:build !windows

package cmd

import (
	"context"

	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/device"
)

// availableShells is empty outside Windows.
func availableShells(context.Context) []string { return nil }

// setWindowsShell does nothing outside Windows: device.shell is the shell
// itself.
func setWindowsShell(context.Context, *config.Config, *device.Options) error { return nil }
