package chat_test

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/chat"
	"github.com/USA-RedDragon/talaria/internal/daemonstate"
	"github.com/USA-RedDragon/talaria/internal/hermesbuild"
	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/internal/oci"
	"github.com/USA-RedDragon/talaria/internal/oci/ocitest"
)

const (
	secret = "control-secret"
	token  = "per-run"

	hermesVersion = "0.22.7"

	parserTitle = "fix the parser"
	argResume   = "--resume"
	argYolo     = "--yolo"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Hermes Agent v0.22.7 (2026.9.24)\nInstall directory: /x\n":                             hermesVersion,
		"Hermes Agent v0.22.7 · stable\n":                                                       hermesVersion,
		"Hermes Agent v0.22.7 (2026.9.24) · stable · local b200f5bc60e3\nInstall method: git\n": hermesVersion,
		"Hermes Agent v0.22.7+3.gabc1234 (2026.9.24) · main":                                    "0.22.7+3.gabc1234",
	}
	for out, want := range cases {
		got, err := chat.ParseVersion(out)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %q, %v; want %q", out, got, err, want)
		}
	}
	if _, err := chat.ParseVersion("usage: hermes\n"); !errors.Is(err, chat.ErrNoVersion) {
		t.Errorf("no version: %v", err)
	}
}

func TestSameVersion(t *testing.T) {
	t.Parallel()
	if !chat.SameVersion(hermesVersion, "v0.22.7") || !chat.SameVersion("v0.22.7", "v0.22.7") {
		t.Error("same release reported different")
	}
	if chat.SameVersion("0.22.6", "v0.22.7") || chat.SameVersion("0.22.7+3.gabc", "v0.22.7") {
		t.Error("different releases reported same")
	}
}

func TestEnvAndArgv(t *testing.T) {
	t.Parallel()
	env := chat.Env([]string{"PATH=/usr/bin", chat.GatewayURLEnv + "=ws://elsewhere", "HOME=/home/alice"}, "127.0.0.1:41733", "a+b")
	want := []string{"PATH=/usr/bin", "HOME=/home/alice", chat.GatewayURLEnv + "=ws://127.0.0.1:41733/api/ws?token=a%2Bb"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env %q", env)
	}
	if got := chat.Argv("hermes", []string{argResume, "s1"}); strings.Join(got, " ") != "hermes --tui --resume s1" {
		t.Fatalf("argv %q", got)
	}
}

type fixture struct {
	hub     *hubapi.Client
	control *chat.Control
	mu      sync.Mutex
	created int
	cwds    []string
	deleted []string
	lookups []string
	// noLatest makes a latest lookup find nothing.
	noLatest bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hubapi.PathWhoami {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(hubapi.Whoami{HermesVersion: "v0.22.7", Placeholder: "/srv/ph"})
	}))
	t.Cleanup(hub.Close)
	f.hub = hubapi.New(hub.URL, "hdt_test", "test")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /control/tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body struct {
			Cwd string `json:"cwd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.created++
		f.cwds = append(f.cwds, body.Cwd)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	})
	mux.HandleFunc("POST /control/lookup", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Session string `json:"session"`
			Latest  bool   `json:"latest"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lookups = append(f.lookups, body.Session)
		noLatest := f.noLatest
		f.mu.Unlock()
		sid := map[string]string{"s1": "s1", parserTitle: "s-parser"}[body.Session]
		if body.Latest && !noLatest {
			sid = "s-latest"
		}
		if sid == "" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": sid})
	})
	mux.HandleFunc("DELETE /control/tokens/{token}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ctl := httptest.NewServer(mux)
	t.Cleanup(ctl.Close)
	f.control = &chat.Control{State: &daemonstate.State{Splice: strings.TrimPrefix(ctl.URL, "http://"), ControlSecret: secret}}
	return f
}

func (f *fixture) params(hermes, dir string) chat.Params {
	return chat.Params{Hermes: hermes, Hub: f.hub, Control: f.control, Dir: dir, Env: []string{"PATH=" + os.Getenv("PATH")},
		Signals: make(chan os.Signal)}
}

func TestResumeTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args   []string
		rest   []string
		name   string
		resume bool
	}{
		{[]string{argYolo}, []string{argYolo}, "", false},
		{[]string{"-c"}, nil, "", true},
		{[]string{"-c", argYolo}, []string{argYolo}, "", true},
		{[]string{"--continue"}, nil, "", true},
		{[]string{"-c", parserTitle, argYolo}, []string{argYolo}, parserTitle, true},
		{[]string{"-cfoo"}, nil, "foo", true},
		{[]string{"--continue=bar"}, nil, "bar", true},
		{[]string{"--cont", "baz"}, nil, "baz", true},
		{[]string{argResume, "s1"}, nil, "s1", true},
		{[]string{"-r", "s1"}, nil, "s1", true},
		{[]string{"-rs1"}, nil, "s1", true},
		{[]string{"--resume=s1"}, nil, "s1", true},
		{[]string{"--resu", "s1"}, nil, "s1", true},
		{[]string{argResume, "Latest"}, nil, "", true},
		{[]string{"-c", "-r", "s1"}, nil, "s1", true},
		{[]string{"-r", "s1", "-c", "other"}, nil, "s1", true},
		{[]string{argResume}, []string{argResume}, "", false},
		{[]string{argYolo, "--", "-c"}, []string{argYolo, "--", "-c"}, "", false},
		{[]string{"--config", "x"}, []string{"--config", "x"}, "", false},
	}
	for _, tc := range cases {
		rest, name, resume := chat.ResumeTarget(tc.args)
		if strings.Join(rest, "\x00") != strings.Join(tc.rest, "\x00") || name != tc.name || resume != tc.resume {
			t.Errorf("ResumeTarget(%q) = %q, %q, %v; want %q, %q, %v", tc.args, rest, name, resume, tc.rest, tc.name, tc.resume)
		}
	}
}

// TestRunManagedBuild is not parallel: it execs a launcher that the pull
// just wrote, which a fork in a parallel test could hold open
// (go.dev/issue/22315).
func TestRunManagedBuild(t *testing.T) { //nolint:paralleltest // see above
	if runtime.GOOS != "linux" {
		t.Skip("managed builds run on Linux only")
	}
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "out")
	launcher := `#!/bin/sh
printf '%s\n' "$*" "$HOME" "$HERMES_HOME" "$` + chat.GatewayURLEnv + `" > '` + out + `'
[ -e /proc/self/fd/3 ] && echo fd3 >> '` + out + `'
exit 3
`
	reg := ocitest.New(t, "nousresearch/hermes-agent")
	img := reg.Push(t, oci.MediaOCILayerGzip, []string{"HERMES_HOME=/opt/data"}, ocitest.Layer(t,
		ocitest.Entry{Name: "opt/hermes/libexec/hermes", Body: launcher, Mode: 0o755},
		ocitest.Entry{Name: "opt/hermes/.venv/bin/hermes", Type: tar.TypeSymlink, Link: "../../libexec/hermes"},
	))
	builds := &hermesbuild.Store{Dir: filepath.Join(t.TempDir(), "hermes"), HTTP: reg.Client(), Arch: "amd64",
		Log: slog.New(slog.DiscardHandler)}
	p := f.params("/nonexistent/hermes", "")
	p.Builds, p.Image = builds, reg.Reference(img.Index)
	p.Args = []string{argYolo}
	code, err := chat.Run(t.Context(), p)
	if err != nil || code != 3 {
		t.Fatalf("Run = %d, %v; want 3", code, err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--tui " + argYolo, builds.Home(), builds.HermesHome(),
		"ws://" + f.control.State.Splice + "/api/ws?token=" + token, "fd3"}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); !slices.Equal(got, want) {
		t.Fatalf("TUI saw %q, want %q", got, want)
	}
	if err := builds.Remove(p.Image); err != nil {
		t.Errorf("Remove after the chat ended: %v (the in-use mark outlived the TUI)", err)
	}
}

// hangupAddr returns the address of a server that drops every connection
// without an answer. It stays up for the whole test: a closed listener's
// port is free again at once, and parallel tests' httptest servers (in this
// process or another package's) take it.
func hangupAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestControlNotRunning(t *testing.T) {
	t.Parallel()
	c := &chat.Control{State: &daemonstate.State{Splice: hangupAddr(t), ControlSecret: secret}}
	if _, err := c.NewToken(t.Context(), "/home/alice"); !errors.Is(err, chat.ErrNotRunning) {
		t.Fatalf("NewToken: %v", err)
	}
}
