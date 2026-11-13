//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/USA-RedDragon/talaria/internal/tokenstore"
)

func TestSetupCommandPairsWithDefaultHub(t *testing.T) {
	t.Parallel()
	hub := &pairHub{t: t}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	root := NewCommand("0.9.0", "test", WithDefaultHub(srv.URL), withFakeSystem(home))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(setupArgs(dir, "--no-shell-check", "--name", pairedName))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	rec, err := tokenstore.New(false, filepath.Join(dir, "state", "token")).Load(srv.URL)
	if err != nil || rec.Token != pairedToken {
		t.Fatalf("token: %+v, %v", rec, err)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".config", "talaria", "config.yaml"))
	if err != nil || !strings.Contains(string(cfg), "url: "+srv.URL) || !strings.Contains(string(cfg), "- "+home) {
		t.Fatalf("config %q, %v", cfg, err)
	}
	if fi, err := os.Stat(filepath.Join(home, setup.BinDir, setup.BinaryName)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary: %v", err)
	}
	if st, err := setup.LoadState(filepath.Join(dir, "state")); err != nil || st.PairedHub != srv.URL {
		t.Fatalf("state %+v, %v", st, err)
	}

	root = NewCommand("0.9.0", "test", withFakeSystem(home))
	root.SetOut(&out)
	root.SetArgs(setupArgs(dir, "--uninstall"))
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := tokenstore.New(false, filepath.Join(dir, "state", "token")).Load(srv.URL); !errors.Is(err, tokenstore.ErrNoToken) {
		t.Errorf("token after uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".talaria")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("binary directory after uninstall: %v", err)
	}
}
