package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/chat"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
)

const (
	secret = "control-secret"
	token  = "per-run-token" //nolint:gosec // test fixture, not a credential

	hermesVersion = "0.22.7"
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
	if got := chat.Argv("hermes", []string{"--resume", "s1"}); strings.Join(got, " ") != "hermes --tui --resume s1" {
		t.Fatalf("argv %q", got)
	}
}

// fakeHermes writes a POSIX sh script that prints version for --version
// and otherwise runs body.
func fakeHermes(t *testing.T, version, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hermes")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then\n  printf 'Hermes Agent v%s (2026.9.24)\\nInstall directory: /opt/hermes\\n' '" +
		version + "'\n  exit 0\nfi\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // the fake hermes script must be executable
		t.Fatal(err)
	}
	return path
}

type fixture struct {
	hub     *hubapi.Client
	control *chat.Control
	mu      sync.Mutex
	created int
	cwds    []string
	deleted []string
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

func (f *fixture) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, append([]string(nil), f.deleted...)
}

func (f *fixture) params(hermes, dir string) chat.Params {
	return chat.Params{Hermes: hermes, Hub: f.hub, Control: f.control, Dir: dir, Env: []string{"PATH=" + os.Getenv("PATH")},
		Signals: make(chan os.Signal)}
}

func TestRunStartsTUI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "out")
	hermes := fakeHermes(t, hermesVersion, `printf '%s\n' "$*" "$`+chat.GatewayURLEnv+`" "$(pwd -P)" > '`+out+`'
exit 7`)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := f.params(hermes, dir)
	p.Args = []string{"--resume", "s1"}
	code, err := chat.Run(t.Context(), p)
	if err != nil || code != 7 {
		t.Fatalf("Run = %d, %v; want 7", code, err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantURL := "ws://" + f.control.State.Splice + "/api/ws?token=" + token
	if len(lines) != 3 || lines[0] != "--tui --resume s1" || lines[1] != wantURL || lines[2] != dir {
		t.Fatalf("TUI saw %q", lines)
	}
	if created, deleted := f.counts(); created != 1 || len(deleted) != 1 || deleted[0] != token {
		t.Fatalf("created %d deleted %q", created, deleted)
	}
	f.mu.Lock()
	cwds := append([]string(nil), f.cwds...)
	f.mu.Unlock()
	if len(cwds) != 1 || cwds[0] != dir {
		t.Fatalf("token cwds %q, want %q", cwds, dir)
	}
}

func TestRunVersionMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hermes := fakeHermes(t, "0.22.6", "exit 0")
	_, err := chat.Run(t.Context(), f.params(hermes, ""))
	if !errors.Is(err, chat.ErrVersionMismatch) {
		t.Fatalf("Run: %v", err)
	}
	if created, _ := f.counts(); created != 0 {
		t.Fatal("a token was created despite the mismatch")
	}
	p := f.params(hermes, "")
	p.AllowVersionMismatch = true
	if code, err := chat.Run(t.Context(), p); err != nil || code != 0 {
		t.Fatalf("Run with the override = %d, %v", code, err)
	}
	if created, deleted := f.counts(); created != 1 || len(deleted) != 1 {
		t.Fatalf("created %d deleted %q", created, deleted)
	}
}

func TestRunForwardsSignals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ready := filepath.Join(t.TempDir(), "ready")
	hermes := fakeHermes(t, hermesVersion, `trap 'exit 3' INT
trap 'exit 42' TERM
: > '`+ready+`'
while :; do sleep 0.05; done`)
	sigs := make(chan os.Signal, 2)
	p := f.params(hermes, "")
	p.Signals = sigs
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := chat.Run(context.Background(), p)
		done <- result{code, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake TUI did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sigs <- syscall.SIGINT
	sigs <- syscall.SIGTERM
	select {
	case r := <-done:
		if r.err != nil || r.code != 42 {
			t.Fatalf("Run = %d, %v; want 42 (SIGINT not forwarded, SIGTERM forwarded)", r.code, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, deleted := f.counts(); len(deleted) != 1 {
		t.Fatalf("deleted %q", deleted)
	}
}

func TestRunSignaledExitCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hermes := fakeHermes(t, hermesVersion, "kill -KILL $$")
	code, err := chat.Run(t.Context(), f.params(hermes, ""))
	if err != nil || code != 128+int(syscall.SIGKILL) {
		t.Fatalf("Run = %d, %v", code, err)
	}
}

func TestReadState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := chat.ReadState(dir, "https://hub.example.com"); !errors.Is(err, chat.ErrNotRunning) {
		t.Fatalf("missing file: %v", err)
	}
	st := daemonstate.State{PID: os.Getpid(), Splice: "127.0.0.1:1", ControlSecret: "s", Hub: "https://hub.example.com"}
	if err := daemonstate.Write(dir, st); err != nil {
		t.Fatal(err)
	}
	if got, err := chat.ReadState(dir, "https://hub.example.com/"); err != nil || got.Splice != st.Splice {
		t.Fatalf("live state: %+v, %v", got, err)
	}
	if _, err := chat.ReadState(dir, "https://other.example.com"); !errors.Is(err, chat.ErrOtherHub) {
		t.Fatalf("other hub: %v", err)
	}
	dead := exec.CommandContext(t.Context(), "true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	st.PID = dead.Process.Pid
	if err := daemonstate.Write(dir, st); err != nil {
		t.Fatal(err)
	}
	if _, err := chat.ReadState(dir, ""); !errors.Is(err, chat.ErrNotRunning) {
		t.Fatalf("stale pid: %v", err)
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
