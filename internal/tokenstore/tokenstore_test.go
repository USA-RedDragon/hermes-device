package tokenstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/tokenstore"
	"github.com/zalando/go-keyring"
)

type fakeKeyring struct {
	m      map[string]string
	fail   bool
	getErr error
}

func (f *fakeKeyring) Get(s, u string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	if v, ok := f.m[s+"/"+u]; ok && !f.fail {
		return v, nil
	}
	return "", keyring.ErrNotFound
}

func (f *fakeKeyring) Set(s, u, p string) error {
	if f.fail {
		return errors.New("no secret service")
	}
	f.m[s+"/"+u] = p
	return nil
}

func (f *fakeKeyring) Delete(s, u string) error {
	if _, ok := f.m[s+"/"+u]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.m, s+"/"+u)
	return nil
}

func record() tokenstore.Record {
	return tokenstore.Record{Hub: "https://hub.example.com", Token: "hdt_x", DeviceID: "d1", DeviceName: "desktop"}
}

func TestKeyring(t *testing.T) {
	t.Parallel()
	kr := &fakeKeyring{m: map[string]string{}}
	rec := record()
	s := &tokenstore.Store{Keyring: kr, File: filepath.Join(t.TempDir(), "token")}
	where, err := s.Save(rec)
	if err != nil || where != "keyring" {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if _, ok := kr.m["talaria/hub.example.com"]; !ok {
		t.Errorf("keyring = %v", kr.m)
	}
	got, err := s.Load(rec.Hub)
	if err != nil || *got != rec {
		t.Errorf("Load = %+v, %v", got, err)
	}
	if err := s.Delete(rec.Hub); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(rec.Hub); !errors.Is(err, tokenstore.ErrNoToken) {
		t.Errorf("after delete: %v", err)
	}
}

func TestFileFallback(t *testing.T) {
	t.Parallel()
	rec := record()
	file := filepath.Join(t.TempDir(), "sub", "token")
	s := &tokenstore.Store{Keyring: &fakeKeyring{m: map[string]string{}, fail: true}, File: file}
	where, err := s.Save(rec)
	if err != nil || where != file {
		t.Fatalf("Save = %q, %v", where, err)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", fi, err)
	}
	got, err := s.Load(rec.Hub)
	if err != nil || got.Token != "hdt_x" {
		t.Errorf("Load = %+v, %v", got, err)
	}
	if _, err := s.Load("https://other.example.com"); !errors.Is(err, tokenstore.ErrNoToken) {
		t.Errorf("other hub: %v", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(rec.Hub); err == nil {
		t.Error("loaded a world-readable token file")
	}
}

func TestLoadCopiesLegacyKeyringEntry(t *testing.T) {
	t.Parallel()
	kr := &fakeKeyring{m: map[string]string{
		"hermes-device/hub.example.com": `{"hub":"https://hub.example.com","token":"t","device_id":"d1"}`,
	}}
	s := &tokenstore.Store{Keyring: kr, File: filepath.Join(t.TempDir(), "token")}
	r, err := s.Load("https://hub.example.com")
	if err != nil || r.Token != "t" || r.DeviceID != "d1" {
		t.Fatalf("Load = %+v, %v", r, err)
	}
	if _, ok := kr.m["hermes-device/hub.example.com"]; !ok {
		t.Error("legacy entry removed")
	}
	if _, ok := kr.m["talaria/hub.example.com"]; !ok {
		t.Error("no talaria entry")
	}
}

// A keychain that is there but cannot be read (macOS over SSH) is an
// error of its own, so setup does not pair the computer a second time.
func TestLoadLockedKeychain(t *testing.T) {
	t.Parallel()
	locked := errors.New("exit status 36")
	s := &tokenstore.Store{
		Keyring: &fakeKeyring{m: map[string]string{}, getErr: locked}, KeyringAlwaysThere: true,
		File: filepath.Join(t.TempDir(), "token"),
	}
	_, err := s.Load(record().Hub)
	if !errors.Is(err, tokenstore.ErrKeyring) || !errors.Is(err, locked) || errors.Is(err, tokenstore.ErrNoToken) {
		t.Fatalf("Load = %v, want ErrKeyring wrapping %v", err, locked)
	}
	// A token file still wins.
	if _, err := (&tokenstore.Store{File: s.File}).Save(record()); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(record().Hub); err != nil || got.Token != record().Token {
		t.Fatalf("Load with a file = %+v, %v", got, err)
	}
	// Where a keyring may be missing (Linux without Secret Service), an
	// error still means no token.
	s = &tokenstore.Store{Keyring: &fakeKeyring{m: map[string]string{}, getErr: locked}, File: filepath.Join(t.TempDir(), "token")}
	if _, err := s.Load(record().Hub); !errors.Is(err, tokenstore.ErrNoToken) {
		t.Fatalf("Load without KeyringAlwaysThere = %v, want ErrNoToken", err)
	}
}
