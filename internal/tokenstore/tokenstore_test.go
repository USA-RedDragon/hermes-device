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
	if f.getErr != nil {
		return f.getErr
	}
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

// A keyring that cannot be reached (Windows Credential Manager over SSH)
// does not stop Delete of a token that is in the file, but Delete reports
// it when there is no file, as the token may then be in the keyring.
func TestDeleteWithUnreachableKeyring(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "token")
	kr := &fakeKeyring{m: map[string]string{}, fail: true}
	s := &tokenstore.Store{Keyring: kr, File: file}
	if _, err := s.Save(record()); err != nil {
		t.Fatal(err)
	}
	kr.getErr = errors.New("A specified logon session does not exist")
	if err := s.Delete(record().Hub); err != nil {
		t.Fatalf("Delete with the token in the file: %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file left: %v", err)
	}
	if err := s.Delete(record().Hub); err == nil {
		t.Error("Delete without a file hid the keyring error")
	}
}
