//go:build unix

package diskfree

import "syscall"

// Available is the space at path that an unprivileged user can use, in
// bytes.
func Available(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec,unconvert // Bsize is positive; its type differs per OS
}

// Sync flushes every file system's buffers to disk.
func Sync() { syscall.Sync() }
