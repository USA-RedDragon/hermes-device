package chat_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

// fakeHermes writes a POSIX sh script that prints version for --version
// and otherwise runs body.
func fakeHermes(t *testing.T, version, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hermes")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then\n  printf 'Hermes Agent v%s (2026.9.24)\\nInstall directory: /opt/hermes\\n' '" +
		version + "'\n  exit 0\nfi\n" + body + "\n"
	// A child forked by a parallel test while the file is open for writing
	// holds that descriptor until it execs, and exec of the script then
	// fails with ETXTBSY (go.dev/issue/22315). Forks take ForkLock for
	// writing, so none can start until the file is closed.
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte(script), 0o600)
	if err == nil {
		err = os.Chmod(path, 0o700)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
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
	p.Args = []string{argResume, "s1"}
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

func TestRunResolvesResume(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "out")
	hermes := fakeHermes(t, hermesVersion, `printf '%s\n' "$*" > '`+out+`'`)
	dir := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-c", argYolo}, "--tui --resume s-latest --yolo"},
		{[]string{"--continue", parserTitle}, "--tui --resume s-parser"},
		{[]string{argResume, "latest"}, "--tui --resume s-latest"},
	} {
		p := f.params(hermes, dir)
		p.Args = tc.args
		if code, err := chat.Run(t.Context(), p); err != nil || code != 0 {
			t.Fatalf("Run(%q) = %d, %v", tc.args, code, err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(data)); got != tc.want {
			t.Fatalf("Run(%q): TUI saw %q, want %q", tc.args, got, tc.want)
		}
	}
	// A name the lookup does not find goes to the splice as sent.
	p := f.params(hermes, dir)
	p.Args = []string{"-c", "no such chat"}
	if code, err := chat.Run(t.Context(), p); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if data, _ := os.ReadFile(out); strings.TrimSpace(string(data)) != "--tui --resume no such chat" {
		t.Fatalf("TUI saw %q", data)
	}
	f.mu.Lock()
	f.noLatest = true
	f.mu.Unlock()
	p = f.params(hermes, dir)
	p.Args = []string{"-c"}
	if _, err := chat.Run(t.Context(), p); !errors.Is(err, chat.ErrNoSession) {
		t.Fatalf("no latest session: %v", err)
	}
	f.mu.Lock()
	lookups := append([]string(nil), f.lookups...)
	f.mu.Unlock()
	if strings.Join(lookups, "|") != "|fix the parser||no such chat|" {
		t.Fatalf("lookups %q", lookups)
	}
	if _, deleted := f.counts(); len(deleted) != 5 {
		t.Fatalf("tokens deleted %d times, want 5", len(deleted))
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

func TestRunManagedWithoutThisPlatformUsesHermes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := ocitest.New(t, "nousresearch/hermes-agent")
	img := reg.Push(t, oci.MediaOCILayerGzip, nil, ocitest.Layer(t, ocitest.Entry{Name: "opt/hermes/f", Body: "x"}))
	p := f.params(fakeHermes(t, hermesVersion, "exit 5"), "")
	p.Builds = &hermesbuild.Store{Dir: t.TempDir(), HTTP: reg.Client(), Arch: "riscv64", Log: slog.New(slog.DiscardHandler)}
	p.Image = reg.Reference(img.Index)
	p.Log = slog.New(slog.DiscardHandler)
	if code, err := chat.Run(t.Context(), p); err != nil || code != 5 {
		t.Fatalf("Run = %d, %v; want chat.hermes to run", code, err)
	}
}

func TestRunImageWithoutDigestUsesHermes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hermes := fakeHermes(t, "0.22.6", "exit 0")
	p := f.params(hermes, "")
	p.Builds = &hermesbuild.Store{Dir: t.TempDir()}
	p.Image = "registry.example.com/hermes:v0.23.1"
	p.Log = slog.New(slog.DiscardHandler)
	if _, err := chat.Run(t.Context(), p); !errors.Is(err, chat.ErrVersionMismatch) {
		t.Fatalf("Run: %v, want the version check of chat.hermes", err)
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
