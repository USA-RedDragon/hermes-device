package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/client"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/protocol"
	"github.com/coder/websocket"
)

type fakeServer struct {
	t       *testing.T
	token   string
	conns   atomic.Int32
	handler func(ctx context.Context, conn *websocket.Conn, hello protocol.Hello)
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	f.conns.Add(1)
	ctx := r.Context()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var hello protocol.Hello
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != protocol.TypeHello {
		f.t.Errorf("bad hello %q", data)
		return
	}
	if hello.Token != f.token {
		_ = conn.Close(protocol.CloseUnauthorized, "bad token")
		return
	}
	writeJSON(ctx, conn, protocol.Welcome{Type: protocol.TypeWelcome, Name: hello.Name})
	f.handler(ctx, conn, hello)
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = conn.Write(ctx, websocket.MessageText, b)
}

type result struct {
	output string
	exit   protocol.Exit
}

func readUntilExit(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) result {
	t.Helper()
	var r result
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env protocol.Envelope
		_ = json.Unmarshal(data, &env)
		switch env.Type {
		case protocol.TypeOutput:
			var o protocol.Output
			_ = json.Unmarshal(data, &o)
			if o.ID == id {
				r.output += o.Data
			}
		case protocol.TypeExit:
			var e protocol.Exit
			_ = json.Unmarshal(data, &e)
			if e.ID == id {
				r.exit = e
				return r
			}
		}
	}
}

func newConfig(t *testing.T, url, root string) *config.Config {
	t.Helper()
	return &config.Config{
		LogLevel: "debug",
		Server:   config.Server{URL: "ws" + strings.TrimPrefix(url, "http")},
		Device:   config.Device{Name: "test-pc", Token: "secret"},
		Roots:    []string{root},
		Shell:    "bash",
		Reconnect: config.Reconnect{
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		},
	}
}

func startClient(t *testing.T, cfg *config.Config) (chan error, context.CancelFunc) {
	t.Helper()
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return done, cancel
}

func TestExecRefuseAndKill(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	finished := make(chan struct{})
	fs := &fakeServer{t: t, token: "secret"}
	fs.handler = func(ctx context.Context, conn *websocket.Conn, hello protocol.Hello) {
		defer close(finished)
		resolved, _ := filepath.EvalSymlinks(root)
		if len(hello.Roots) != 1 || hello.Roots[0] != resolved || hello.Name != "test-pc" {
			t.Errorf("hello = %+v", hello)
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "pwd; exit 3", Cwd: root, Timeout: 10})
		r := readUntilExit(ctx, t, conn, "1")
		if r.exit.Code != 3 || strings.TrimSpace(r.output) != resolved {
			t.Errorf("exec 1 = %+v", r)
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "2", Cmd: "pwd", Cwd: outside, Timeout: 10})
		r = readUntilExit(ctx, t, conn, "2")
		if r.exit.Code != protocol.ExitRefused || !strings.Contains(r.exit.Error, "outside the allowed roots") || r.output != "" {
			t.Errorf("exec 2 = %+v", r)
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "3", Cmd: "echo started; sleep 60", Cwd: root, Timeout: 120})
		time.Sleep(300 * time.Millisecond)
		start := time.Now()
		writeJSON(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: "3"})
		r = readUntilExit(ctx, t, conn, "3")
		if !r.exit.Killed || r.exit.Code != 130 || !strings.Contains(r.output, "started") || time.Since(start) > 5*time.Second {
			t.Errorf("exec 3 = %+v after %s", r, time.Since(start))
		}
	}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	_, cancel := startClient(t, newConfig(t, srv.URL, root))
	defer cancel()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out")
	}
}

func TestUnauthorizedStops(t *testing.T) {
	t.Parallel()
	fs := &fakeServer{t: t, token: "other", handler: func(context.Context, *websocket.Conn, protocol.Hello) {}}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	done, cancel := startClient(t, newConfig(t, srv.URL, t.TempDir()))
	defer cancel()
	select {
	case err := <-done:
		if !errors.Is(err, client.ErrUnauthorized) {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client kept running")
	}
}

func TestReconnects(t *testing.T) {
	t.Parallel()
	fs := &fakeServer{t: t, token: "secret"}
	fs.handler = func(context.Context, *websocket.Conn, protocol.Hello) {}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	_, cancel := startClient(t, newConfig(t, srv.URL, t.TempDir()))
	defer cancel()
	deadline := time.Now().Add(10 * time.Second)
	for fs.conns.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fs.conns.Load() < 3 {
		t.Errorf("connections = %d, want at least 3", fs.conns.Load())
	}
}
