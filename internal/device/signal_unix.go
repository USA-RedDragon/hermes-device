//go:build unix

package device

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// signalName is the SIG name of sig, such as SIGKILL.
func signalName(sig syscall.Signal) string { return unix.SignalName(sig) }
