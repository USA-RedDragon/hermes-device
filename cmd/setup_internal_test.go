package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/setup"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
)

var errNoProgram = errors.New("no such program")

type nullRunner struct{}

func (nullRunner) Output(context.Context, []string, string, ...string) ([]byte, error) {
	return nil, errNoProgram
}

func (nullRunner) Start(string, ...string) error { return errNoProgram }

// withFakeSystem points setup at a temporary home with no service manager,
// Firefox or shells.
func withFakeSystem(home string) Option {
	return func(b *base) {
		b.system = func() (*setup.System, error) {
			return &setup.System{
				GOOS: "linux", Home: home, ConfigDir: filepath.Join(home, ".config"), CacheDir: filepath.Join(home, ".cache"),
				Run: nullRunner{}, Getenv: func(string) string { return "" },
				LookPath: func(string) (string, error) { return "", errNoProgram },
				In:       strings.NewReader(""), Out: &bytes.Buffer{}, Now: time.Now,
			}, nil
		}
	}
}

func setupArgs(dir string, extra ...string) []string {
	return append([]string{
		"setup", "--config", filepath.Join(dir, "home", ".config", "talaria", "config.yaml"),
		"--store.keyring=false", "--store.state-dir", filepath.Join(dir, "state"), "--wait", "0",
	}, extra...)
}

func TestSetupCommandNeedsHub(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := NewCommand("0.9.0", "test", withFakeSystem(filepath.Join(dir, "home")))
	root.SetOut(&bytes.Buffer{})
	root.SetArgs(setupArgs(dir))
	if err := root.ExecuteContext(t.Context()); !errors.Is(err, ErrNoHub) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupCommandHubMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "home", ".config", "talaria", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("hub:\n  url: https://hub.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand("0.9.0", "test", withFakeSystem(filepath.Join(dir, "home")))
	root.SetOut(&bytes.Buffer{})
	root.SetArgs(setupArgs(dir, "https://other.example.com"))
	if err := root.ExecuteContext(t.Context()); !errors.Is(err, ErrHubMismatch) {
		t.Fatalf("err = %v", err)
	}
}

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

func TestDaemonBrowserStatus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := daemonBrowserStatus(t.Context(), dir); !errors.Is(err, daemonstate.ErrNotRunning) {
		t.Fatalf("no daemon: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == splice.PathControlBrowser {
			_, _ = w.Write([]byte(`{"enabled":true,"connected":true}`))
		}
	}))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	if err := daemonstate.Write(dir, daemonstate.State{PID: 1, Splice: ln.Addr().String(), ControlSecret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	st, err := daemonBrowserStatus(t.Context(), dir)
	if err != nil || !st.Enabled || !st.Connected {
		t.Fatalf("status %+v, %v", st, err)
	}
}

func TestPairIfNeededKeepsAcceptedToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hubapi.PathWhoami {
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSONBody(t, w, hubapi.Whoami{DeviceID: pairedID, DeviceName: pairedName})
	}))
	defer srv.Close()
	st := tokenstore.New(false, filepath.Join(t.TempDir(), "token"))
	if _, err := st.Save(tokenstore.Record{Hub: srv.URL, Token: pairedToken}); err != nil {
		t.Fatal(err)
	}
	b := &base{version: "0.9.0"}
	var out bytes.Buffer
	paired, err := b.pairIfNeeded(t.Context(), &out, nil, srv.URL, st, pairedName)
	if err != nil || paired || !strings.Contains(out.String(), pairedID) {
		t.Fatalf("pairIfNeeded = %t, %v, %q", paired, err, out.String())
	}
}
