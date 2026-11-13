package diskfree

import (
	"errors"
	"syscall"
)

var errBlockSize = errors.New("statfs gave no block size")

// Available is the space at path that an unprivileged user can use, in
// bytes.
func Available(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, errBlockSize
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Sync flushes every file system's buffers to disk.
func Sync() { syscall.Sync() }
