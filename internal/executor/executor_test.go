package executor_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/executor"
)

type sink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *sink) write(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(b)
}

func (s *sink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func run(t *testing.T, req executor.Request) (executor.Result, string) {
	t.Helper()
	var out sink
	p, err := executor.Start("bash", req, out.write)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func TestStdin(t *testing.T) {
	t.Parallel()
	in := "hello stdin\n"
	res, out := run(t, executor.Request{Cmd: "cat", Cwd: t.TempDir(), Stdin: &in})
	if out != in || res.Code != 0 {
		t.Errorf("output = %q, code %d", out, res.Code)
	}
}

func TestLoginShell(t *testing.T) {
	t.Parallel()
	_, out := run(t, executor.Request{Cmd: "shopt -q login_shell && echo login || echo nologin", Cwd: t.TempDir(), Login: true})
	if !strings.Contains(out, "login") || strings.Contains(out, "nologin") {
		t.Errorf("output = %q", out)
	}
	_, out = run(t, executor.Request{Cmd: "shopt -q login_shell && echo login || echo nologin", Cwd: t.TempDir()})
	if !strings.Contains(out, "nologin") {
		t.Errorf("output = %q", out)
	}
}

func TestStreamsBeforeExit(t *testing.T) {
	t.Parallel()
	got := make(chan string, 4)
	p, err := executor.Start("bash", executor.Request{Cmd: "echo first; sleep 5", Cwd: t.TempDir()}, func(b []byte) { got <- string(b) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if !strings.Contains(s, "first") {
			t.Errorf("chunk = %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no output before exit")
	}
	p.Kill()
	res, _ := p.Wait(context.Background())
	if !res.Killed || res.Code != executor.ExitKilled {
		t.Errorf("result = %+v", res)
	}
}
