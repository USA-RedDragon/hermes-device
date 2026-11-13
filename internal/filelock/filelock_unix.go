//go:build unix

package filelock

import (
	"errors"
	"os"
	"syscall"
)

func how(m Mode) int {
	if m == Exclusive {
		return syscall.LOCK_EX
	}
	return syscall.LOCK_SH
}

// Lock waits for a lock of mode m on f.
func Lock(f *os.File, m Mode) error {
	for {
		err := syscall.Flock(int(f.Fd()), how(m))
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// TryLock takes a lock of mode m on f, or returns ErrLocked at once.
func TryLock(f *os.File, m Mode) error {
	err := syscall.Flock(int(f.Fd()), how(m)|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

// Unlock drops the lock on f.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
