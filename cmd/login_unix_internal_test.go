//go:build unix

package cmd

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/tokenstore"
)

func TestLoginCommandStoresTokenFile(t *testing.T) {
	t.Parallel()
	hub := &pairHub{t: t}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("log-level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand("v0.2.0", "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"login", "--config", cfgFile, "--hub.url", srv.URL, "--store.keyring=false", "--store.state-dir", stateDir, "--name", pairedName})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := hub.name.Load().(string); got != pairedName {
		t.Errorf("suggested name = %q", got)
	}
	file := filepath.Join(stateDir, "token")
	text := out.String()
	for _, want := range []string{"https://hub.example.com/device/pair\n", "WDJB-MJHT", file} {
		if !strings.Contains(text, want) {
			t.Errorf("output %q lacks %q", text, want)
		}
	}
	rec, err := tokenstore.New(false, file).Load(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := tokenstore.Record{Hub: srv.URL, Token: pairedToken, DeviceID: pairedID, DeviceName: pairedName, Backend: testUser}
	if *rec != want {
		t.Errorf("record = %+v, want %+v", *rec, want)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, %v", fi, err)
	}
}
