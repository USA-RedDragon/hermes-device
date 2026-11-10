package cmd

import (
	"context"
	"os/signal"
	"syscall"
)

func signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
}
