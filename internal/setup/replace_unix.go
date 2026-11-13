//go:build !windows

package setup

import "os"

// replaceFile moves src over dst.
func replaceFile(src, dst string) error { return os.Rename(src, dst) }

// RemoveOldBinaries does nothing outside Windows, where replaceFile leaves
// nothing behind.
func RemoveOldBinaries(string) {}
