//go:build !windows

package cmd

import "context"

// withParent only adds a cancel outside Windows, where systemd and launchd
// stop the daemon themselves.
func withParent(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
