//go:build windows

package diskfree

import "golang.org/x/sys/windows"

// Available is the space at path that the calling user can use (it
// honors disk quotas), in bytes.
func Available(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// Sync does nothing on Windows: there is no call that flushes every file
// system without administrator rights, and NTFS writes metadata through
// its log. Callers that need a file on disk flush that file.
func Sync() {}
