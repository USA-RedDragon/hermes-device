//go:build !unix

package claudeusage

import "io/fs"

func fileID(fs.FileInfo) (ino, dev uint64) { return 0, 0 }
