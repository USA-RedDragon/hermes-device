//go:build windows

package launch

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detached starts Firefox in a process group of its own, without the
// daemon's console, so it outlives the daemon.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
