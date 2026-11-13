//go:build unix

package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/executor"
)

func TestKillStopsProcessGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	p, err := executor.Start("bash", executor.Request{
		Cmd: "sleep 60 & echo $! > " + pidFile + "; wait", Cwd: dir,
	}, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	var childPid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			for _, c := range strings.TrimSpace(string(b)) {
				childPid = childPid*10 + int(c-'0')
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPid == 0 {
		t.Fatal("child never started")
	}
	start := time.Now()
	p.Kill()
	res, _ := p.Wait(context.Background())
	if time.Since(start) > executor.KillGrace+2*time.Second {
		t.Errorf("kill took %s", time.Since(start))
	}
	if !res.Killed {
		t.Errorf("result = %+v", res)
	}
	deadline = time.Now().Add(executor.KillGrace + 2*time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(childPid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("child %d still alive after kill", childPid)
}

func TestCwdOutputAndExitCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res, out := run(t, executor.Request{Cmd: "pwd; echo err >&2; exit 7", Cwd: dir})
	if res.Code != 7 {
		t.Errorf("code = %d, want 7", res.Code)
	}
	// pwd may print the path as given or with its symlinks resolved (on
	// macOS the temporary directory is under /var, a link to /private/var)
	resolved, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(out, resolved) && !strings.Contains(out, dir) || !strings.Contains(out, "err") {
		t.Errorf("output = %q", out)
	}
}
