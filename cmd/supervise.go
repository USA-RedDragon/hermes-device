package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/talaria/internal/legacy"
	"github.com/USA-RedDragon/talaria/internal/proctree"
	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/USA-RedDragon/talaria/internal/userdir"
)

// ServiceFlag makes run the supervisor of the Windows scheduled task: it
// runs talaria run again as a child, logs to a file and starts the child
// again when it exits.
const ServiceFlag = "service"

// Supervisor timing: restartDelay after a crash, as systemd's RestartSec;
// none after an auto-update or a Restart.
const (
	restartDelay = 5 * time.Second
	maxLogSize   = 10 << 20
)

// supervise runs exe with args until ctx ends, again after each exit. dir
// is talaria's config directory, for the log and the pid file.
func supervise(ctx context.Context, exe string, args []string, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(dir, setup.ServiceLogFile)
	pidPath := filepath.Join(dir, setup.ServicePIDFile)
	for {
		log, err := openLog(logPath)
		if err != nil {
			return err
		}
		code, err := superviseOnce(ctx, exe, args, log, pidPath)
		_ = os.Remove(pidPath)
		delay := restartDelay
		if code == exitRestart {
			delay = 0
		}
		_, _ = fmt.Fprintf(log, "talaria service: the daemon exited (code %d, %v); starting it again in %s\n", code, err, delay)
		_ = log.Close()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// superviseOnce runs the daemon once, in a process tree that ends with
// the supervisor, and returns its exit code.
func superviseOnce(ctx context.Context, exe string, args []string, log io.Writer, pidPath string) (int, error) {
	var tree atomic.Pointer[proctree.Tree]
	child := exec.CommandContext(ctx, exe, args...)
	child.Env = append(os.Environ(), setup.ServiceEnv+"="+setup.ServiceTask)
	child.Stdout, child.Stderr = log, log
	// When ctx ends, the whole tree goes, not only the daemon.
	child.Cancel = func() error {
		if t := tree.Load(); t != nil {
			return t.Kill()
		}
		return child.Process.Kill()
	}
	proctree.Prepare(child)
	if err := child.Start(); err != nil {
		return -1, err
	}
	t, err := proctree.Attach(child)
	if err != nil {
		_ = child.Wait()
		return -1, err
	}
	tree.Store(t)
	defer t.Release()
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	err = child.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// openLog opens the service log for appending, after moving a log larger
// than maxLogSize to <log>.1.
func openLog(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// serviceExe is the binary the supervisor runs: the path the task started
// it with, which after an update names the new binary.
func serviceExe(executable func() (string, error)) (string, error) {
	if len(os.Args) > 0 && filepath.IsAbs(os.Args[0]) {
		return os.Args[0], nil
	}
	return executable()
}

// childRunArgs are run's arguments without --service.
func childRunArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a != "--"+ServiceFlag {
			out = append(out, a)
		}
	}
	return out
}

// runSupervisor is run --service.
func runSupervisor(ctx context.Context, b *base) error {
	if runtime.GOOS != goosWindows {
		return errors.New("--service is for the Windows scheduled task; systemd and launchd restart run themselves")
	}
	exe, err := serviceExe(b.executable)
	if err != nil {
		return err
	}
	base, err := userdir.Config()
	if err != nil {
		return err
	}
	args := childRunArgs(os.Args[1:])
	slog.Info("Supervising the daemon for the scheduled task", "binary", exe, "args", args)
	ctx, stop := signalContext(ctx)
	defer stop()
	ctx, cancel := withParent(ctx)
	defer cancel()
	return supervise(ctx, exe, args, filepath.Join(base, legacy.Name))
}
