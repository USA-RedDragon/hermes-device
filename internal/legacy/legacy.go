// Package legacy finds the files of releases from before the rename of
// hermes-device to talaria, until the migration moves them.
package legacy

import (
	"os"
	"path/filepath"
)

// Name is the program's name, and OldName its name before the rename.
const (
	Name    = "talaria"
	OldName = "hermes-device"
)

// Dir returns dir's legacy twin (base hermes-device instead of talaria)
// when dir does not exist and the twin is a directory, else dir.
func Dir(dir string) string {
	if filepath.Base(dir) != Name {
		return dir
	}
	if _, err := os.Lstat(dir); err == nil {
		return dir
	}
	old := filepath.Join(filepath.Dir(dir), OldName)
	if fi, err := os.Lstat(old); err == nil && fi.IsDir() {
		return old
	}
	return dir
}
