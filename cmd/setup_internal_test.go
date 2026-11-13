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

	"github.com/USA-RedDragon/talaria/internal/daemonstate"
	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/USA-RedDragon/talaria/internal/splice"
	"github.com/USA-RedDragon/talaria/internal/tokenstore"
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

type startRecorder struct {
	nullRunner
	started []string
}

func (r *startRecorder) Start(name string, args ...string) error {
	r.started = append(r.started, name+" "+strings.Join(args, " "))
	return nil
}

func TestOpenURLSkipsSSHOnMacOS(t *testing.T) {
	t.Parallel()
	const u = "https://hub.example.com/device/pair"
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
		want       bool
	}{
		{"mac desktop", goosDarwin, nil, true},
		{"mac over ssh", goosDarwin, map[string]string{"SSH_CONNECTION": "192.0.2.1 50000 192.0.2.2 22"}, false},
		{"mac over ssh with a tty", goosDarwin, map[string]string{"SSH_TTY": "/dev/ttys001"}, false},
		{"linux desktop", goosLinux, map[string]string{"DISPLAY": ":0"}, true},
		{"linux headless", goosLinux, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &startRecorder{}
			sys := &setup.System{
				GOOS: tc.goos, Run: r,
				Getenv:   func(k string) string { return tc.env[k] },
				LookPath: func(p string) (string, error) { return "/usr/bin/" + p, nil },
			}
			openURL(sys, u)
			if got := len(r.started) == 1; got != tc.want {
				t.Fatalf("opened = %v (%q), want %v", got, r.started, tc.want)
			}
		})
	}
}
