//go:build unix

package headless

import (
	"os/exec"
	"syscall"
)

// group puts Firefox and its child processes in a process group of their own,
// so one signal reaches them all.
func group() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func terminate(cmd *exec.Cmd, graceful bool) {
	sig := syscall.SIGKILL
	if graceful {
		sig = syscall.SIGTERM
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}
