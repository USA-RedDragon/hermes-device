// Package executor runs one bash command per request in its own process
// tree (a process group, or a Job Object on Windows), streams merged stdout
// and stderr, and kills the tree on request.
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

	"github.com/USA-RedDragon/talaria/internal/proctree"
	"github.com/USA-RedDragon/talaria/protocol"
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
const ExitKilled = protocol.ExitKilled

// Request is one command to run.
type Request struct {
	Cmd     string
	Cwd     string
	Login   bool
	Timeout time.Duration
	Stdin   *string
	Env     []string
	// Argv, when set, builds the command line instead of <shell> [-l] -c
	// <cmd>, as for a Windows shell (internal/winshell).
	Argv func(cmd string, login bool, cwd string) []string
	// Scope, when set, runs the command in a systemd scope of its own.
	Scope *Scope
}

// Result is how a command ended.
type Result struct {
	Code   int
	Killed bool
	// Signal ended the shell; 0 when it exited.
	Signal syscall.Signal
	// OOM is true when the command's scope used more than its memory cap
	// and the kernel's OOM killer killed a process in it. MemoryMax is
	// that cap in bytes.
	OOM       bool
	MemoryMax int64
}

// Process is a running command.
type Process struct {
	cmd      *exec.Cmd
	done     chan struct{}
	killOnce sync.Once
	mu       sync.Mutex
	killed   bool
	result   Result
	// timer kills the command at its timeout plus TimeoutGrace; nil
	// without a timeout or after Detach.
	timer *time.Timer
	// tree is the command's process tree.
	tree *proctree.Tree
	// scope and unit are the systemd scope the command runs in, if any.
	scope *Scope
	unit  string
}

// Start runs req with shell. Output chunks go to out from one goroutine, in
// order, until Wait returns.
func Start(shell string, req Request, out func([]byte)) (*Process, error) {
	return start(shell, req, out, TimeoutGrace)
}

func start(shell string, req Request, out func([]byte), grace time.Duration) (*Process, error) {
	argv := []string{shell}
	if req.Login {
		argv = append(argv, "-l")
	}
	argv = append(argv, "-c", req.Cmd)
	if req.Argv != nil {
		argv = req.Argv(req.Cmd, req.Login, req.Cwd)
	}
	unit := ""
	if req.Scope != nil {
		unit = scopeUnit()
		argv = req.Scope.argv(unit, argv)
	}
	// No context kills the command: Kill stops its process group.
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: argv}
	cmd.Dir = req.Cwd
	cmd.Env = req.Env
	proctree.Prepare(cmd)

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
	tree, err := proctree.Attach(cmd)
	if err != nil {
		_ = cmd.Wait()
		_ = pr.Close()
		return nil, err
	}

	p := &Process{cmd: cmd, tree: tree, done: make(chan struct{}), scope: req.Scope, unit: unit}
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

	if req.Timeout > 0 {
		p.mu.Lock()
		p.timer = time.AfterFunc(req.Timeout+grace, p.Kill)
		p.mu.Unlock()
	}

	go func() {
		err := cmd.Wait()
		p.Detach()
		p.mu.Lock()
		killed := p.killed
		p.mu.Unlock()
		if !killed {
			// Processes the command left running keep running, as a
			// process group's members outlive its leader.
			p.tree.Release()
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
		res := Result{Code: exitCode(err), Signal: signalOf(err)}
		// A kill that lost the race to a natural exit keeps the real exit code.
		res.Killed = p.killed && proctree.Killed(err)
		p.mu.Unlock()
		if res.Killed {
			res.Code = ExitKilled
		} else if p.scope != nil && res.Signal != 0 && p.scope.oomKilled(p.unit) {
			res.OOM, res.MemoryMax = true, p.scope.MemoryMax
		}
		p.mu.Lock()
		p.result = res
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

func signalOf(err error) syscall.Signal {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal()
		}
	}
	return 0
}

// Kill sends SIGTERM to the process tree, then SIGKILL after KillGrace. On
// Windows both end the whole tree at once.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		p.mu.Lock()
		p.killed = true
		p.mu.Unlock()
		_ = p.tree.Terminate()
		go func() {
			select {
			case <-p.done:
			case <-time.After(KillGrace):
			}
			_ = p.tree.Kill()
			p.tree.Release()
		}()
	})
}

// Detach stops the device's own timeout: the command then runs until it
// exits or is killed.
func (p *Process) Detach() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

// Pid returns the process id, which is also the process group id on Unix.
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
