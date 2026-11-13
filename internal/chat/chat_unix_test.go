//go:build unix

package chat_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/chat"
	"github.com/USA-RedDragon/talaria/internal/daemonstate"
	"github.com/USA-RedDragon/talaria/internal/hermesbuild"
	"github.com/USA-RedDragon/talaria/internal/oci"
	"github.com/USA-RedDragon/talaria/internal/oci/ocitest"
)

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

func TestRunSignaledExitCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hermes := fakeHermes(t, hermesVersion, "kill -KILL $$")
	code, err := chat.Run(t.Context(), f.params(hermes, ""))
	if err != nil || code != 128+int(syscall.SIGKILL) {
		t.Fatalf("Run = %d, %v", code, err)
	}
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

func (f *fixture) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, append([]string(nil), f.deleted...)
}
