//go:build !unix

package headless

import (
	"os/exec"
	"syscall"
)

func group() *syscall.SysProcAttr { return nil }

func terminate(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }
