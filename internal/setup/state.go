package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// StateFileName is the file in the state directory where setup records
// what it did, so that uninstall reverses only that.
const StateFileName = "setup.json"

// Edit is a block setup added to a file the user owns.
type Edit struct {
	File string `json:"file"`
	// Created is true when setup created the file.
	Created bool `json:"created,omitempty"`
	// Backup is the copy of the file from before the edit.
	Backup string `json:"backup,omitempty"`
}

// State is what setup did.
type State struct {
	// Binary is the binary setup copied to the stable path.
	Binary string `json:"binary,omitempty"`
	// Config and ConfigSHA256 are the config file setup wrote and its hash
	// then, so uninstall keeps a file the user changed since.
	Config       string `json:"config,omitempty"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	// ConfigBackup is the user's config file that setup replaced.
	ConfigBackup string `json:"config_backup,omitempty"`
	// Service and ServiceSHA256 are the unit or launchd agent file setup
	// wrote and its hash then; ServiceBackup is the file it replaced.
	// ServiceOn is true when setup started the service.
	Service       string `json:"service,omitempty"`
	ServiceSHA256 string `json:"service_sha256,omitempty"`
	ServiceBackup string `json:"service_backup,omitempty"`
	ServiceOn     bool   `json:"service_on,omitempty"`
	// RunningConfig is the hash of the config file when setup last started
	// or restarted the service, so a later run restarts it after a change.
	RunningConfig string `json:"running_config,omitempty"`
	// NativeHost is the native messaging host manifest setup created.
	NativeHost string `json:"native_host,omitempty"`
	// PairedHub is the hub whose device token setup stored.
	PairedHub string `json:"paired_hub,omitempty"`
	// Edits are the shell files setup changed.
	Edits []Edit `json:"edits,omitempty"`
}

// LoadState reads the state file in dir. A missing file is an empty state.
func LoadState(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, StateFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", StateFileName, err)
	}
	return &st, nil
}

// Save writes the state file into dir.
func (st *State) Save(dir string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, StateFileName), append(data, '\n'), 0o600)
}

// edit returns the recorded edit of file, or nil.
func (st *State) edit(file string) *Edit {
	for i := range st.Edits {
		if st.Edits[i].File == file {
			return &st.Edits[i]
		}
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, so a reader never sees half a file.
// A symbolic link at path is kept: the file it points to is replaced. A
// link that points nowhere is an error.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("%s is a symbolic link to a missing file: %w", path, err)
		}
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// backup copies path to path.talaria-<time>.bak and returns the copy.
func (s *System) backup(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s.talaria-%s", path, s.Now().Format("20060102-150405"))
	var f *os.File
	var dst string
	for i := 0; f == nil; i++ {
		dst = base + ".bak"
		if i > 0 {
			dst = fmt.Sprintf("%s-%d.bak", base, i)
		}
		f, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		if err != nil && (!errors.Is(err, fs.ErrExist) || i >= 99) {
			return "", err
		}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	return dst, f.Close()
}
