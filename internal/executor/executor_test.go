package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/executor"
)

type sink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *sink) write(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(b)
}

func (s *sink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func run(t *testing.T, req executor.Request) (executor.Result, string) {
	t.Helper()
	var out sink
	p, err := executor.Start("bash", req, out.write)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
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

func TestStdin(t *testing.T) {
	t.Parallel()
	in := "hello stdin\n"
	_, out := run(t, executor.Request{Cmd: "cat", Cwd: t.TempDir(), Stdin: &in})
	if out != in {
		t.Errorf("output = %q", out)
	}
}

func TestLoginShell(t *testing.T) {
	t.Parallel()
	_, out := run(t, executor.Request{Cmd: "shopt -q login_shell && echo login || echo nologin", Cwd: t.TempDir(), Login: true})
	if !strings.Contains(out, "login") || strings.Contains(out, "nologin") {
		t.Errorf("output = %q", out)
	}
	_, out = run(t, executor.Request{Cmd: "shopt -q login_shell && echo login || echo nologin", Cwd: t.TempDir()})
	if !strings.Contains(out, "nologin") {
		t.Errorf("output = %q", out)
	}
}

func TestStreamsBeforeExit(t *testing.T) {
	t.Parallel()
	got := make(chan string, 4)
	p, err := executor.Start("bash", executor.Request{Cmd: "echo first; sleep 5", Cwd: t.TempDir()}, func(b []byte) { got <- string(b) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if !strings.Contains(s, "first") {
			t.Errorf("chunk = %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no output before exit")
	}
	p.Kill()
	res, _ := p.Wait(context.Background())
	if !res.Killed || res.Code != executor.ExitKilled {
		t.Errorf("result = %+v", res)
	}
}

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
