//go:build unix

package proctree_test

import (
	"errors"
	"syscall"
)

// alive reports whether pid runs. A zombie child of a test process does
// not count: it is reaped by the helper parent's exit or by init.
func alive(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	var ws syscall.WaitStatus
	got, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return got != pid
}
