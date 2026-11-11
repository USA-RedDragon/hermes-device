//go:build unix

package claudeusage

import (
	"io/fs"
	"syscall"
)

// fileID returns the inode and device of a file, which tell a replaced
// file from one that grew.
func fileID(fi fs.FileInfo) (ino, dev uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, uint64(st.Dev) //nolint:unconvert // Dev is int32 on darwin
	}
	return 0, 0
}
