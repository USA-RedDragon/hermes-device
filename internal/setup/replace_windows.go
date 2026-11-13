//go:build windows

package setup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// oldSuffix marks a binary that replaceFile moved aside.
const oldSuffix = ".old"

// replaceFile moves src over dst. Windows cannot replace a program that
// runs, such as the daemon's talaria.exe, but it can rename it: dst moves
// aside to dst.old first (or dst.old-<time> while an older .old still
// runs), and RemoveOldBinaries deletes those once nothing runs them.
func replaceFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if _, serr := os.Lstat(dst); errors.Is(serr, fs.ErrNotExist) {
		return err
	}
	aside := dst + oldSuffix
	if rerr := os.Remove(aside); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		aside += "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if rerr := os.Rename(dst, aside); rerr != nil {
		return errors.Join(err, rerr)
	}
	if rerr := os.Rename(src, dst); rerr != nil {
		_ = os.Rename(aside, dst)
		return rerr
	}
	return nil
}

// RemoveOldBinaries deletes the binaries that replaceFile moved aside next
// to bin. Those that still run stay until the next call.
func RemoveOldBinaries(bin string) {
	matches, _ := filepath.Glob(bin + oldSuffix + "*")
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// removeBinary removes path. While the daemon that the scheduled task
// ran still exits, the file is busy for a moment, so it tries for a few
// seconds. A program that still runs then, such as the talaria.exe that
// runs setup --uninstall itself, cannot be deleted, so it moves to the
// temp folder instead, and removeBinary returns where.
func removeBinary(path string) (string, error) {
	var err error
	for range 20 {
		if err = os.Remove(path); err == nil || errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		time.Sleep(250 * time.Millisecond)
	}
	aside := filepath.Join(os.TempDir(), "talaria-removed-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".exe")
	if rerr := os.Rename(path, aside); rerr != nil {
		return "", errors.Join(err, rerr)
	}
	return aside, nil
}
