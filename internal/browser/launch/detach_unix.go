//go:build unix

package launch

import "syscall"

// detached starts Firefox in a session of its own, so it outlives the
// daemon.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
