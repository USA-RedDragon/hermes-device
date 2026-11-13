//go:build windows

package chat

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is GetExitCodeProcess's code for a running process.
const stillActive = 259

// processAlive reports whether pid names a running process.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // a pid fits
	if err != nil {
		// Access denied means it exists.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do on a close error
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
