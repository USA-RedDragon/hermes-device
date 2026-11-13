//go:build unix

package tokenstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/tokenstore"
)

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
