package device_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

const (
	testToken = "hdt_test"
	testName  = "desktop"
)

type fakeHub struct {
	t           *testing.T
	status      int
	skipWelcome bool
	conns       atomic.Int32
	handler     func(ctx context.Context, conn *websocket.Conn, hello protocol.Hello)
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != hubapi.PathExecLeg {
		http.NotFound(w, r)
		return
	}
	f.conns.Add(1)
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
	if f.status != 0 {
		http.Error(w, `{"error":"unauthorized"}`, f.status)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
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
	if !f.skipWelcome {
		writeJSON(ctx, conn, protocol.Welcome{
			Type: protocol.TypeWelcome, Version: protocol.Version, DeviceID: "d_1", Name: testName, PingInterval: 25,
		})
	}
	if f.handler != nil {
		f.handler(ctx, conn, hello)
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, b)
}

func readMsg(ctx context.Context, conn *websocket.Conn) (string, []byte, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	var env protocol.Envelope
	_ = json.Unmarshal(data, &env)
	return env.Type, data, nil
}

type result struct {
	output string
	exit   protocol.Exit
}

func readUntilExit(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) result {
	t.Helper()
	var r result
	for {
		typ, data, err := readMsg(ctx, conn)
		if err != nil {
			t.Errorf("read: %v", err)
			return r
		}
		switch typ {
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

func options(t *testing.T, url, root string) device.Options {
	t.Helper()
	allow, err := allowlist.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return device.Options{
		Hub: hubapi.New(url, testToken, "hermes-device/test"), Allow: allow, Shell: "bash",
		TempDir: "/home/alice/.cache/hermes-device/tmp", Home: "/home/alice", Hostname: testName, ClientVersion: "v0.2.0",
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	}
}

func start(t *testing.T, opts device.Options) chan error {
	t.Helper()
	c, err := device.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return done
}

func serve(t *testing.T, f *fakeHub) *httptest.Server {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out")
	}
}

func TestHelloFields(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	got := make(chan protocol.Hello, 1)
	f := &fakeHub{handler: func(ctx context.Context, _ *websocket.Conn, hello protocol.Hello) {
		got <- hello
		<-ctx.Done()
	}}
	srv := serve(t, f)
	start(t, options(t, srv.URL, root))
	var hello protocol.Hello
	select {
	case hello = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no hello")
	}
	resolved, _ := filepath.EvalSymlinks(root)
	want := protocol.Hello{
		Type: protocol.TypeHello, Version: 2, Roots: []string{resolved}, TempDir: "/home/alice/.cache/hermes-device/tmp",
		Home: "/home/alice", Hostname: testName, OS: runtime.GOOS, Arch: runtime.GOARCH, Shell: "bash", ClientVersion: "v0.2.0",
	}
	if !reflect.DeepEqual(hello, want) {
		t.Errorf("hello = %+v, want %+v", hello, want)
	}
}

func TestHelloHomeDefault(t *testing.T) {
	t.Parallel()
	opts := options(t, "http://127.0.0.1:1", t.TempDir())
	opts.Home = ""
	c, err := device.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Hello(); h.Home != os.Getenv("HOME") || h.Token != "" || h.Name != "" {
		t.Errorf("hello = %+v", h)
	}
}

func TestExecRefuseAndKill(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	finished := make(chan struct{})
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		defer close(finished)
		resolved, _ := filepath.EvalSymlinks(root)
		stdin := "from stdin"
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "pwd; cat; printf 'caf\\303\\251'; exit 3", Cwd: root, Timeout: 10, Stdin: &stdin})
		r := readUntilExit(ctx, t, conn, "1")
		if r.exit.Code != 3 || r.output != resolved+"\nfrom stdincafé" {
			t.Errorf("exec 1 = %+v", r)
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "2", Cmd: "pwd", Cwd: outside, Timeout: 10})
		r = readUntilExit(ctx, t, conn, "2")
		if r.exit.Code != protocol.ExitRefused || r.output != "" ||
			!strings.HasPrefix(r.exit.Error, "device desktop refused: working directory") || !strings.Contains(r.exit.Error, "outside the allowed roots") {
			t.Errorf("exec 2 = %+v", r)
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "3", Cmd: "echo started; sleep 60", Cwd: root, Timeout: 120})
		var o protocol.Output
		for o.ID != "3" {
			typ, data, err := readMsg(ctx, conn)
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if typ == protocol.TypeOutput {
				_ = json.Unmarshal(data, &o)
			}
		}
		begin := time.Now()
		writeJSON(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: "unknown"})
		writeJSON(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: "3"})
		r = readUntilExit(ctx, t, conn, "3")
		if !r.exit.Killed || r.exit.Code != protocol.ExitKilled || time.Since(begin) > 5*time.Second {
			t.Errorf("exec 3 = %+v after %s", r, time.Since(begin))
		}
	}}
	srv := serve(t, f)
	start(t, options(t, srv.URL, root))
	wait(t, finished)
}

func TestIgnoresUnknownTypes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	finished := make(chan struct{})
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		defer close(finished)
		writeJSON(ctx, conn, protocol.Envelope{Type: protocol.TypeResume})
		writeJSON(ctx, conn, map[string]any{"type": "future", "x": 1})
		_ = conn.Write(ctx, websocket.MessageText, []byte("not json"))
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "echo ok", Cwd: root, Timeout: 10})
		if r := readUntilExit(ctx, t, conn, "1"); r.exit.Code != 0 || r.output != "ok\n" {
			t.Errorf("exec = %+v", r)
		}
	}}
	srv := serve(t, f)
	start(t, options(t, srv.URL, root))
	wait(t, finished)
	if n := f.conns.Load(); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
}

func TestReconnects(t *testing.T) {
	t.Parallel()
	f := &fakeHub{}
	srv := serve(t, f)
	start(t, options(t, srv.URL, t.TempDir()))
	waitFor(t, "3 connections", func() bool { return f.conns.Load() >= 3 })
}

func TestUpgrade401Stops(t *testing.T) {
	t.Parallel()
	f := &fakeHub{status: http.StatusUnauthorized}
	srv := serve(t, f)
	done := start(t, options(t, srv.URL, t.TempDir()))
	select {
	case err := <-done:
		if !errors.Is(err, hubapi.ErrUnauthorized) {
			t.Errorf("Run = %v", err)
		}
		done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("client kept running")
	}
	if n := f.conns.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestUpgradeOtherStatusRetries(t *testing.T) {
	t.Parallel()
	f := &fakeHub{status: http.StatusBadGateway}
	srv := serve(t, f)
	start(t, options(t, srv.URL, t.TempDir()))
	waitFor(t, "3 attempts", func() bool { return f.conns.Load() >= 3 })
}

func TestCloseCodes(t *testing.T) {
	t.Parallel()
	stops := map[string]struct {
		code        websocket.StatusCode
		skipWelcome bool
		want        error
	}{
		"4401":                {protocol.CloseUnauthorized, false, hubapi.ErrUnauthorized},
		"4401 before welcome": {protocol.CloseUnauthorized, true, hubapi.ErrUnauthorized},
		"4409":                {protocol.CloseReplaced, false, device.ErrReplaced},
		"4426":                {protocol.CloseUnsupportedVersion, false, device.ErrUnsupportedVersion},
		"4426 before welcome": {protocol.CloseUnsupportedVersion, true, device.ErrUnsupportedVersion},
	}
	for name, tc := range stops {
		t.Run("stop "+name, func(t *testing.T) {
			t.Parallel()
			f := &fakeHub{skipWelcome: tc.skipWelcome, handler: func(_ context.Context, conn *websocket.Conn, _ protocol.Hello) {
				_ = conn.Close(tc.code, "test")
			}}
			srv := serve(t, f)
			done := start(t, options(t, srv.URL, t.TempDir()))
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Errorf("Run = %v, want %v", err, tc.want)
				}
				done <- err
			case <-time.After(10 * time.Second):
				t.Fatal("client kept running")
			}
			if n := f.conns.Load(); n != 1 {
				t.Errorf("attempts = %d, want 1", n)
			}
		})
	}
	retries := map[string]websocket.StatusCode{
		"1000": websocket.StatusNormalClosure, "1001": websocket.StatusGoingAway, "1008": websocket.StatusPolicyViolation,
		"4403": protocol.CloseAssertionRefused, "4408": protocol.ClosePingTimeout, "4502": protocol.CloseBackendUnavailable,
	}
	for name, code := range retries {
		t.Run("retry "+name, func(t *testing.T) {
			t.Parallel()
			f := &fakeHub{handler: func(_ context.Context, conn *websocket.Conn, _ protocol.Hello) {
				_ = conn.Close(code, "test")
			}}
			srv := serve(t, f)
			start(t, options(t, srv.URL, t.TempDir()))
			waitFor(t, "3 connections", func() bool { return f.conns.Load() >= 3 })
		})
	}
}

func TestAnswersPing(t *testing.T) {
	t.Parallel()
	finished := make(chan struct{})
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		defer close(finished)
		writeJSON(ctx, conn, protocol.Ping{Type: protocol.TypePing, Seq: 17})
		for {
			typ, data, err := readMsg(ctx, conn)
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if typ == protocol.TypePong {
				var p protocol.Ping
				_ = json.Unmarshal(data, &p)
				if p.Seq != 17 {
					t.Errorf("pong seq = %d", p.Seq)
				}
				return
			}
		}
	}}
	srv := serve(t, f)
	start(t, options(t, srv.URL, t.TempDir()))
	wait(t, finished)
}

func TestSendsPingsAndStaysUpWithPongs(t *testing.T) {
	t.Parallel()
	var pings atomic.Int32
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		var last int64
		for {
			typ, data, err := readMsg(ctx, conn)
			if err != nil {
				return
			}
			if typ == protocol.TypePing {
				var p protocol.Ping
				_ = json.Unmarshal(data, &p)
				if p.Seq <= last {
					t.Errorf("ping seq %d after %d", p.Seq, last)
				}
				last = p.Seq
				pings.Add(1)
				writeJSON(ctx, conn, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
			}
		}
	}}
	srv := serve(t, f)
	opts := options(t, srv.URL, t.TempDir())
	opts.PingInterval = 20 * time.Millisecond
	opts.SilenceTimeout = 200 * time.Millisecond
	start(t, opts)
	waitFor(t, "20 pings", func() bool { return pings.Load() >= 20 })
	if n := f.conns.Load(); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
}

func TestDropsSilentPlugin(t *testing.T) {
	t.Parallel()
	closed := make(chan websocket.StatusCode, 4)
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				closed <- websocket.CloseStatus(err)
				return
			}
		}
	}}
	srv := serve(t, f)
	opts := options(t, srv.URL, t.TempDir())
	opts.PingInterval = 20 * time.Millisecond
	opts.SilenceTimeout = 150 * time.Millisecond
	begin := time.Now()
	start(t, opts)
	select {
	case code := <-closed:
		if code != protocol.ClosePingTimeout {
			t.Errorf("close code = %d, want %d", code, protocol.ClosePingTimeout)
		}
		if d := time.Since(begin); d < opts.SilenceTimeout {
			t.Errorf("dropped after %s, before the silence timeout", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("silent plugin not dropped")
	}
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
}

func TestControlPingTimeout(t *testing.T) {
	t.Parallel()
	f := &fakeHub{handler: func(ctx context.Context, _ *websocket.Conn, _ protocol.Hello) {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}}
	srv := serve(t, f)
	opts := options(t, srv.URL, t.TempDir())
	opts.PingInterval = 20 * time.Millisecond
	opts.PingTimeout = 50 * time.Millisecond
	opts.SilenceTimeout = time.Hour
	start(t, opts)
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
}

func TestNoWelcomeRetries(t *testing.T) {
	t.Parallel()
	f := &fakeHub{skipWelcome: true, handler: func(ctx context.Context, _ *websocket.Conn, _ protocol.Hello) {
		<-ctx.Done()
	}}
	srv := serve(t, f)
	opts := options(t, srv.URL, t.TempDir())
	opts.HelloTimeout = 50 * time.Millisecond
	start(t, opts)
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
}

func TestDisconnectKillsCommands(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	f := &fakeHub{}
	var first atomic.Bool
	f.handler = func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		if !first.CompareAndSwap(false, true) {
			<-ctx.Done()
			return
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "echo $$ > " + pidFile + "; echo go; sleep 60", Cwd: root, Timeout: 120})
		for {
			typ, _, err := readMsg(ctx, conn)
			if err != nil || typ == protocol.TypeOutput {
				return
			}
		}
	}
	srv := serve(t, f)
	start(t, options(t, srv.URL, root))
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to die", func() bool { return syscall.Kill(pid, 0) != nil })
}
