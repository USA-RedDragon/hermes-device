// Package executor runs one bash command per request in its own process
// group, streams merged stdout and stderr, and kills the group on request.
package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// KillGrace is how long a killed process group gets after SIGTERM before
// SIGKILL.
const KillGrace = 2 * time.Second

// TimeoutGrace is added to a request's timeout before the device kills the
// command itself. The server enforces the real timeout; this only stops a
// command whose server went away.
const TimeoutGrace = 10 * time.Second

// ExitKilled is the exit code reported for a killed command, the same code
// Hermes uses for an interrupted command.
const ExitKilled = 130

// Request is one command to run.
type Request struct {
	Cmd     string
	Cwd     string
	Login   bool
	Timeout time.Duration
	Stdin   *string
	Env     []string
}

// Result is how a command ended.
type Result struct {
	Code   int
	Killed bool
}

// Process is a running command.
type Process struct {
	cmd      *exec.Cmd
	done     chan struct{}
	killOnce sync.Once
	mu       sync.Mutex
	killed   bool
	result   Result
}

// Start runs req with shell. Output chunks go to out from one goroutine, in
// order, until Wait returns.
func Start(shell string, req Request, out func([]byte)) (*Process, error) {
	args := []string{}
	if req.Login {
		args = append(args, "-l")
	}
	args = append(args, "-c", req.Cmd)
	cmd := exec.Command(shell, args...) //nolint:gosec // running the agent's command is the purpose
	cmd.Dir = req.Cwd
	cmd.Env = req.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if req.Stdin != nil {
		cmd.Stdin = strings.NewReader(*req.Stdin)
	}
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	_ = pw.Close()

	p := &Process{cmd: cmd, done: make(chan struct{})}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				out(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()

	var timer *time.Timer
	if req.Timeout > 0 {
		timer = time.AfterFunc(req.Timeout+TimeoutGrace, p.Kill)
	}

	go func() {
		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		// Background children may keep the pipe open; do not wait for them
		// longer than a moment once bash itself has exited.
		select {
		case <-readDone:
		case <-time.After(500 * time.Millisecond):
			_ = pr.Close()
			<-readDone
		}
		_ = pr.Close()
		p.mu.Lock()
		code := exitCode(err)
		// A kill that lost the race to a natural exit keeps the real exit code.
		killed := p.killed && signaled(err)
		if killed {
			code = ExitKilled
		}
		p.result = Result{Code: code, Killed: killed}
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

func signaled(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled() || ws.ExitStatus() == 128+int(syscall.SIGTERM)
		}
	}
	return false
}

// Kill sends SIGTERM to the process group, then SIGKILL after KillGrace.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		p.mu.Lock()
		p.killed = true
		p.mu.Unlock()
		pgid := p.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func() {
			select {
			case <-p.done:
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			case <-time.After(KillGrace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}()
	})
}

// Pid returns the process id, which is also the process group id.
func (p *Process) Pid() int {
	return p.cmd.Process.Pid
}

// Wait blocks until the command exits or ctx ends.
func (p *Process) Wait(ctx context.Context) (Result, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.result, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
