//go:build unix

package proctree

import (
	"errors"
	"os/exec"
	"syscall"
)

// KillExitCode is unused on Unix, where a killed command ends by a signal.
const KillExitCode = -1

// Tree is a process group.
type Tree struct{ pgid int }

// SysProcAttr puts the command in a new process group.
func SysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// Prepare sets cmd's SysProcAttr to SysProcAttr.
func Prepare(cmd *exec.Cmd) { cmd.SysProcAttr = SysProcAttr() }

// Attach returns the tree of a started command. The process group id is
// the head's pid.
func Attach(cmd *exec.Cmd) (*Tree, error) {
	if cmd.Process == nil {
		return nil, errors.New("proctree: command not started")
	}
	return &Tree{pgid: cmd.Process.Pid}, nil
}

// Terminate asks every process of the tree to stop (SIGTERM).
func (t *Tree) Terminate() error { return syscall.Kill(-t.pgid, syscall.SIGTERM) }

// Kill stops every process of the tree at once (SIGKILL).
func (t *Tree) Kill() error { return syscall.Kill(-t.pgid, syscall.SIGKILL) }

// Release lets go of the tree; a no-op on Unix.
func (t *Tree) Release() {}

// Killed reports whether err is the exit of a process that a signal or
// Terminate or Kill ended.
func Killed(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled() || ws.ExitStatus() == 128+int(syscall.SIGTERM)
		}
	}
	return false
}
