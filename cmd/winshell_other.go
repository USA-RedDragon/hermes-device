//go:build !windows

package cmd

import (
	"context"

	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/winshell"
)

// windowsShell is nil outside Windows: device.shell is the shell itself.
func windowsShell(context.Context, *config.Config) (*winshell.Shell, error) { return nil, nil }
