//go:build windows

package cmd

import (
	"context"
	"log/slog"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// withParent returns a context that ends when the process that started
// this one exits. The scheduled task runs the supervisor through conhost
// --headless, and ending the task (schtasks /End, End in Task Scheduler,
// setup --uninstall) ends only conhost: without this the supervisor and
// the daemon would keep running.
func withParent(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	ppid := os.Getppid()
	if ppid <= 0 || ppid > math.MaxUint32 {
		return ctx, cancel
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(ppid))
	if err != nil {
		slog.Warn("Cannot watch the process that started the supervisor; ending the scheduled task will not stop the daemon", "pid", ppid, "error", err)
		return ctx, cancel
	}
	go func() {
		defer func() { _ = windows.CloseHandle(h) }()
		if ev, err := windows.WaitForSingleObject(h, windows.INFINITE); err == nil && ev == windows.WAIT_OBJECT_0 {
			slog.Info("The scheduled task ended; stopping the daemon", "parent", ppid)
			cancel()
		}
	}()
	return ctx, cancel
}
