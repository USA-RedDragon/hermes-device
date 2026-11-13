//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// allBytes locks the whole file, also bytes past its end.
const allBytes = ^uint32(0)

func flags(m Mode) uint32 {
	if m == Exclusive {
		return windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return 0
}

// Lock waits for a lock of mode m on f.
func Lock(f *os.File, m Mode) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), flags(m), 0, allBytes, allBytes, ol)
}

// TryLock takes a lock of mode m on f, or returns ErrLocked at once.
func TryLock(f *os.File, m Mode) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags(m)|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, allBytes, allBytes, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

// Unlock drops the lock on f.
func Unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, ol)
}
