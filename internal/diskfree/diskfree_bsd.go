//go:build unix && !linux

package diskfree

import "syscall"

// Available is the space at path that an unprivileged user can use, in
// bytes.
func Available(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Sync flushes every file system's buffers to disk. It reports no error:
// the callers only make a written file durable before they go on.
func Sync() { _ = syscall.Sync() }
