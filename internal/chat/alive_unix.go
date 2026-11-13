//go:build unix

package chat

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a process.
func processAlive(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }
