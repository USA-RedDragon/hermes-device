// Package daemonstate reads and writes daemon.json, through which chat
// finds the run daemon's splice (docs/protocol.md section 5.5).
package daemonstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
)

// FileName is the state file's name in the state directory.
const FileName = "daemon.json"

// ErrNotRunning is returned when no daemon state file exists.
var ErrNotRunning = errors.New("hermes-device run is not running")

// State is the content of daemon.json.
type State struct {
	PID           int    `json:"pid"`
	Splice        string `json:"splice"`
	ControlSecret string `json:"control_secret"`
	DeviceID      string `json:"device_id"`
	Hub           string `json:"hub"`
}

// Write writes st into dir with mode 0600.
func Write(dir string, st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return tokenstore.WritePrivate(filepath.Join(dir, FileName), data)
}

// Read reads the state file in dir.
func Read(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotRunning
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &st, nil
}

// Remove deletes the state file in dir if it still names pid.
func Remove(dir string, pid int) error {
	st, err := Read(dir)
	if err != nil || st.PID != pid {
		return nil //nolint:nilerr // nothing of ours to remove
	}
	return os.Remove(filepath.Join(dir, FileName))
}
