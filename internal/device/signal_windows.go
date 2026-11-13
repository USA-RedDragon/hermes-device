//go:build windows

package device

import "syscall"

// signalName is empty: on Windows a command never ends by a signal.
func signalName(syscall.Signal) string { return "" }
