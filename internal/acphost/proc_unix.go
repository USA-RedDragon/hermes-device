//go:build unix

package acphost

import "syscall"

// reap collects the exit status of the zombie pid.
func reap(pid int) {
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

func signalPid(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
