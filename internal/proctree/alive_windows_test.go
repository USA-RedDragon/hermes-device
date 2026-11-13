//go:build windows

package proctree_test

import "golang.org/x/sys/windows"

const stillActive = 259

func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // a pid fits
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // test cleanup
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
