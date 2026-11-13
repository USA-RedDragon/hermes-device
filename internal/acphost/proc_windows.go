//go:build windows

package acphost

import (
	"os"
	"syscall"
)

// reap does nothing: Windows has no zombies, and findStrays finds none
// there.
func reap(int) {}

func signalPid(pid int, _ syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
