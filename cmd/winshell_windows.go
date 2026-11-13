//go:build windows

package cmd

import (
	"context"
	"errors"
	"log/slog"

	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/device"
	"github.com/USA-RedDragon/talaria/internal/winshell"
)

// availableShells are the shells setup offers here, the default first.
func availableShells(ctx context.Context) []string { return winshell.Detect(ctx).Kinds() }

// setWindowsShell sets the shell that runs agent commands on Windows from
// device.shell. The default, bash, is Git Bash when it is installed, else
// WSL.
func setWindowsShell(ctx context.Context, cfg *config.Config, opts *device.Options) error {
	sh, err := windowsShell(ctx, cfg)
	opts.WinShell = sh
	return err
}

func windowsShell(ctx context.Context, cfg *config.Config) (*winshell.Shell, error) {
	kind := cfg.Device.Shell
	if kind == "" || kind == "bash" {
		if kind = winshell.Detect(ctx).Default(); kind == "" {
			return nil, errors.New("no shell for agent commands: install Git for Windows (winget install Git.Git) " +
				"or a WSL distribution, then set device.shell to git-bash, powershell or wsl")
		}
	}
	sh, err := winshell.Resolve(ctx, kind, cfg.Device.WSLDistro)
	if err != nil {
		return nil, err
	}
	slog.Info("Agent commands run in "+sh.Describe(), "shell", sh.Kind, "bash", sh.Bash, "powershell", sh.PowerShell, "distro", sh.Distro)
	return &sh, nil
}
