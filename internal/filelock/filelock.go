// Package filelock takes advisory whole-file locks: flock on Unix,
// LockFileEx on Windows. A lock belongs to the open file and ends when it
// is closed or unlocked.
package filelock

import "errors"

// ErrLocked is returned by a non-blocking lock that another holder has.
var ErrLocked = errors.New("file is locked")

// Mode is a lock mode.
type Mode int

// Lock modes.
const (
	Shared Mode = iota
	Exclusive
)
