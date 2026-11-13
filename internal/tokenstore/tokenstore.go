// Package tokenstore keeps the device token: in the OS keyring when one is
// available, else in a file with mode 0600.
package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/USA-RedDragon/talaria/internal/legacy"
	"github.com/USA-RedDragon/talaria/internal/userdir"
	"github.com/zalando/go-keyring"
)

// Service is the keyring service name. The account is the hub URL's host.
const Service = "talaria"

// LegacyService is the keyring service name before the rename to talaria.
// Load copies an entry from it to Service.
const LegacyService = "hermes-device"

// ErrNoToken is returned when no token is stored for the hub.
var ErrNoToken = errors.New("no device token stored; run talaria login")

// ErrKeyring is returned when the keyring could not be read and there is
// no token file. On macOS that is the login keychain over SSH: it stays
// locked for that session, even though the token is in it.
var ErrKeyring = errors.New("could not read the device token from the OS keyring")

// Record is what login stores.
type Record struct {
	Hub        string `json:"hub"`
	Token      string `json:"token"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Backend    string `json:"backend"`
}

// Keyring is the subset of go-keyring the store uses.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (osKeyring) Set(s, u, p string) error        { return keyring.Set(s, u, p) }
func (osKeyring) Delete(s, u string) error        { return keyring.Delete(s, u) }

// Store reads and writes the token.
type Store struct {
	// Keyring is nil when the keyring is off.
	Keyring Keyring
	// File is the fallback file.
	File string
	// KeyringAlwaysThere is true where every user has a keyring (macOS). A
	// keyring error other than not found then means it could not be read,
	// not that there is none, so Load reports it instead of ErrNoToken.
	KeyringAlwaysThere bool
}

// New returns a store. useKeyring selects the OS keyring.
func New(useKeyring bool, file string) *Store {
	s := &Store{File: file}
	if useKeyring {
		s.Keyring = osKeyring{}
		s.KeyringAlwaysThere = runtime.GOOS == "darwin"
	}
	return s
}

// DefaultStateDir is talaria in the user config directory, or the
// hermes-device one of a release before the rename until the migration
// moves it.
func DefaultStateDir() (string, error) {
	dir, err := userdir.Config()
	if err != nil {
		return "", err
	}
	return legacy.Dir(filepath.Join(dir, legacy.Name)), nil
}

func account(hub string) (string, error) {
	u, err := url.Parse(hub)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad hub URL %q", hub)
	}
	return u.Host, nil
}

// Save stores r. It returns where: "keyring" or the file path. A keyring
// that fails falls back to the file.
func (s *Store) Save(r Record) (string, error) {
	acct, err := account(r.Hub)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if s.Keyring != nil {
		if err := s.Keyring.Set(Service, acct, string(data)); err == nil {
			if err := os.Remove(s.File); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "keyring", err
			}
			return "keyring", nil
		}
	}
	if err := writePrivate(s.File, data); err != nil {
		return "", err
	}
	return s.File, nil
}

// Load returns the record for hub.
func (s *Store) Load(hub string) (*Record, error) {
	acct, err := account(hub)
	if err != nil {
		return nil, err
	}
	var kerr error
	if s.Keyring != nil {
		v, err := s.Keyring.Get(Service, acct)
		if err == nil {
			var r Record
			if err := json.Unmarshal([]byte(v), &r); err != nil {
				return nil, fmt.Errorf("keyring entry for %s: %w", acct, err)
			}
			return &r, nil
		}
		if s.KeyringAlwaysThere && !errors.Is(err, keyring.ErrNotFound) {
			kerr = err
		}
		if r, ok := s.moveLegacy(acct); ok {
			return r, nil
		}
	}
	data, err := os.ReadFile(s.File)
	if errors.Is(err, os.ErrNotExist) {
		if kerr != nil {
			return nil, fmt.Errorf("%w for %s: %w. Over SSH, macOS keeps the login keychain locked: "+
				"run talaria in Terminal on the Mac, or run security unlock-keychain first", ErrKeyring, acct, kerr)
		}
		return nil, ErrNoToken
	}
	if err != nil {
		return nil, err
	}
	// Windows has no mode bits: a file there reads as 0666, and its ACL,
	// inherited from the user's profile folder, keeps others out.
	if fi, err := os.Stat(s.File); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %o); chmod 600 it", s.File, fi.Mode().Perm())
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", s.File, err)
	}
	if r.Hub != hub {
		return nil, ErrNoToken
	}
	return &r, nil
}

// moveLegacy reads the keyring entry that a release before the rename
// stored, and copies it to Service. The legacy entry stays, so that an
// older binary still finds it; Delete removes both.
func (s *Store) moveLegacy(acct string) (*Record, bool) {
	v, err := s.Keyring.Get(LegacyService, acct)
	if err != nil {
		return nil, false
	}
	var r Record
	if json.Unmarshal([]byte(v), &r) != nil {
		return nil, false
	}
	if err := s.Keyring.Set(Service, acct, v); err != nil {
		slog.Warn("Could not copy the device token to the talaria keyring entry; using the hermes-device entry", "error", err)
		return &r, true
	}
	slog.Info("Copied the device token in the keyring from the hermes-device entry to the talaria entry", "account", acct)
	return &r, true
}

// Delete removes the record for hub from the keyring and the file.
func (s *Store) Delete(hub string) error {
	acct, err := account(hub)
	if err != nil {
		return err
	}
	if s.Keyring != nil {
		for _, svc := range []string{Service, LegacyService} {
			if err := s.Keyring.Delete(svc, acct); err != nil && !errors.Is(err, keyring.ErrNotFound) {
				return err
			}
		}
	}
	if err := os.Remove(s.File); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
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

// WritePrivate writes data to path with mode 0600 (directory 0700),
// atomically.
func WritePrivate(path string, data []byte) error { return writePrivate(path, data) }
