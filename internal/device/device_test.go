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
	// version is the welcome version, protocol.Version when 0. A version
	// 3 welcome is followed by an empty resume unless skipResume is set.
	version    int
	grace      float64
	skipResume bool
	conns      atomic.Int32
	handler    func(ctx context.Context, conn *websocket.Conn, hello protocol.Hello)
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
	version := f.version
	if version == 0 {
		version = protocol.Version
	}
	if !f.skipWelcome {
		writeJSON(ctx, conn, protocol.Welcome{
			Type: protocol.TypeWelcome, Version: version, DeviceID: "d_1", Name: testName, PingInterval: 25,
			GraceSeconds: f.grace,
		})
		if version >= protocol.ResumeVersion && !f.skipResume {
			writeJSON(ctx, conn, protocol.Resume{Type: protocol.TypeResume, Commands: []protocol.ResumeCommand{}})
		}
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
		Type: protocol.TypeHello, Version: 2, MaxVersion: 3, Roots: []string{resolved}, TempDir: "/home/alice/.cache/hermes-device/tmp",
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
		"4427":                {protocol.CloseUnsupportedPlatform, true, device.ErrUnsupportedOS},
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

// resumeHub serves the exec leg with a handler per connection, numbered
// from 1. Each connection gets a version 3 welcome and then the resume
// that resumeFor returns.
type resumeHub struct {
	t         *testing.T
	grace     float64
	n         atomic.Int32
	resumeFor func(n int32) []protocol.ResumeCommand
	handle    func(ctx context.Context, n int32, conn *websocket.Conn)
	refuseAt  int32
}

func (f *resumeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.n.Add(1)
	if f.refuseAt > 0 && n >= f.refuseAt {
		http.Error(w, `{"error":"backend_unavailable"}`, http.StatusBadGateway)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := r.Context()
	if _, _, err := conn.Read(ctx); err != nil {
		return
	}
	writeJSON(ctx, conn, protocol.Welcome{
		Type: protocol.TypeWelcome, Version: protocol.Version, DeviceID: "d_1", Name: testName, GraceSeconds: f.grace,
	})
	var cmds []protocol.ResumeCommand
	if f.resumeFor != nil {
		cmds = f.resumeFor(n)
	}
	writeJSON(ctx, conn, protocol.Resume{Type: protocol.TypeResume, Commands: cmds})
	if f.handle != nil {
		f.handle(ctx, n, conn)
		return
	}
	<-ctx.Done()
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	var data []byte
	waitFor(t, "the pid file", func() bool {
		var err error
		data, err = os.ReadFile(path)
		return err == nil && strings.HasSuffix(string(data), "\n")
	})
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitOutput reads until the output of id holds want.
func waitOutput(ctx context.Context, t *testing.T, conn *websocket.Conn, id, want string) {
	t.Helper()
	var got string
	for !strings.Contains(got, want) {
		typ, data, err := readMsg(ctx, conn)
		if err != nil {
			t.Errorf("read: %v (output so far %q)", err, got)
			return
		}
		if typ == protocol.TypeOutput {
			var o protocol.Output
			_ = json.Unmarshal(data, &o)
			if o.ID == id {
				got += o.Data
			}
		}
	}
}

type resumed struct {
	outputs []protocol.Output
	exit    protocol.Exit
}

func readResumed(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) resumed {
	t.Helper()
	var r resumed
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
				r.outputs = append(r.outputs, o)
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

func TestResumeSendsMissingOutputAndExit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gate := filepath.Join(root, "gate")
	finished := make(chan struct{})
	f := &resumeHub{grace: 30}
	f.resumeFor = func(n int32) []protocol.ResumeCommand {
		if n == 2 {
			return []protocol.ResumeCommand{{ID: "1", Offset: 6}}
		}
		return nil
	}
	f.handle = func(ctx context.Context, n int32, conn *websocket.Conn) {
		switch n {
		case 1:
			writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Timeout: 60, Cwd: root,
				Cmd: "echo first; while [ ! -e " + gate + " ]; do sleep 0.02; done; echo second; exit 7"})
			waitOutput(ctx, t, conn, "1", "first\n")
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Error(err)
			}
			time.Sleep(300 * time.Millisecond)
			_ = conn.Close(websocket.StatusGoingAway, "hub shutting down")
		case 2:
			r := readResumed(ctx, t, conn, "1")
			var out string
			for _, o := range r.outputs {
				if o.Offset < 6 {
					t.Errorf("replayed output at offset %d, before the plugin's offset 6", o.Offset)
				}
				out += o.Data
			}
			if out != "second\n" || r.exit.Code != 7 || r.outputs[0].Offset != 6 {
				t.Errorf("resumed = %+v", r)
			}
			close(finished)
			<-ctx.Done()
		}
	}
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	start(t, options(t, srv.URL, root))
	wait(t, finished)
}

func TestResumeUnknownCommandIsLost(t *testing.T) {
	t.Parallel()
	finished := make(chan struct{})
	f := &resumeHub{grace: 30, resumeFor: func(int32) []protocol.ResumeCommand {
		return []protocol.ResumeCommand{{ID: "gone", Offset: 12}}
	}}
	f.handle = func(ctx context.Context, _ int32, conn *websocket.Conn) {
		defer close(finished)
		r := readUntilExit(ctx, t, conn, "gone")
		if r.exit.Code != protocol.ExitLost || !strings.Contains(r.exit.Error, "no record of this command") || r.output != "" {
			t.Errorf("exit = %+v", r)
		}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	start(t, options(t, srv.URL, t.TempDir()))
	wait(t, finished)
}

// TestResumeKillsUnlisted is the gateway restart case: the new plugin
// process knows none of the old commands, so its resume is empty.
func TestResumeKillsUnlisted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	f := &resumeHub{grace: 30}
	f.handle = func(ctx context.Context, n int32, conn *websocket.Conn) {
		if n == 1 {
			writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Timeout: 60, Cwd: root,
				Cmd: "echo $$ > " + pidFile + "; sleep 60"})
			readPid(t, pidFile)
			_ = conn.Close(websocket.StatusGoingAway, "hub shutting down")
			return
		}
		<-ctx.Done()
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	start(t, options(t, srv.URL, root))
	pid := readPid(t, pidFile)
	waitFor(t, "a second connection", func() bool { return f.n.Load() >= 2 })
	waitFor(t, "the command to die", func() bool { return !alive(pid) })
}

func TestGraceExpiryKills(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	f := &resumeHub{grace: 0.5, refuseAt: 2}
	f.handle = func(ctx context.Context, _ int32, conn *websocket.Conn) {
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Timeout: 60, Cwd: root,
			Cmd: "echo $$ > " + pidFile + "; sleep 60"})
		readPid(t, pidFile)
		_ = conn.Close(websocket.StatusGoingAway, "hub shutting down")
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	start(t, options(t, srv.URL, root))
	pid := readPid(t, pidFile)
	waitFor(t, "a refused reconnect", func() bool { return f.n.Load() >= 2 })
	dropped := time.Now()
	time.Sleep(200 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("the command died before the grace ran out")
	}
	waitFor(t, "the command to die", func() bool { return !alive(pid) })
	if d := time.Since(dropped); d > 5*time.Second {
		t.Errorf("killed %s after the drop, grace is 0.5s", d)
	}
}

func TestServiceRestartKillsAtOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	f := &resumeHub{grace: 60, refuseAt: 2}
	f.handle = func(ctx context.Context, _ int32, conn *websocket.Conn) {
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Timeout: 60, Cwd: root,
			Cmd: "echo $$ > " + pidFile + "; sleep 60"})
		readPid(t, pidFile)
		_ = conn.Close(protocol.CloseServiceRestart, "service restart")
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	start(t, options(t, srv.URL, root))
	pid := readPid(t, pidFile)
	waitFor(t, "the command to die", func() bool { return !alive(pid) })
}

func TestVersion2PluginKillsOnDrop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	f := &fakeHub{version: 2, grace: 60}
	var first atomic.Bool
	f.handler = func(ctx context.Context, conn *websocket.Conn, hello protocol.Hello) {
		if hello.Version != 2 || hello.MaxVersion != protocol.Version {
			t.Errorf("hello versions = %d to %d", hello.Version, hello.MaxVersion)
		}
		if !first.CompareAndSwap(false, true) {
			<-ctx.Done()
			return
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Timeout: 60, Cwd: root,
			Cmd: "echo $$ > " + pidFile + "; sleep 60"})
		readPid(t, pidFile)
	}
	srv := serve(t, f)
	start(t, options(t, srv.URL, root))
	pid := readPid(t, pidFile)
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
	waitFor(t, "the command to die", func() bool { return !alive(pid) })
}

func TestWelcomeVersionMismatchStops(t *testing.T) {
	t.Parallel()
	closed := make(chan error, 1)
	f := &fakeHub{version: 9, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		_, _, err := conn.Read(ctx)
		closed <- err
	}}
	srv := serve(t, f)
	done := start(t, options(t, srv.URL, t.TempDir()))
	select {
	case err := <-done:
		if !errors.Is(err, device.ErrUnsupportedVersion) || !strings.Contains(err.Error(), "speaks protocol 2 to 3") {
			t.Errorf("Run = %v", err)
		}
		done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("client kept running")
	}
	err := <-closed
	var ce websocket.CloseError
	if !errors.As(err, &ce) || int(ce.Code) != protocol.CloseUnsupportedVersion || !strings.Contains(ce.Reason, "chose 9") {
		t.Errorf("close = %v", err)
	}
}

func TestUnsupportedVersionReasonShown(t *testing.T) {
	t.Parallel()
	f := &fakeHub{skipWelcome: true, handler: func(_ context.Context, conn *websocket.Conn, _ protocol.Hello) {
		_ = conn.Close(protocol.CloseUnsupportedVersion, "plugin 0.9.0 speaks protocol 4 to 5")
	}}
	srv := serve(t, f)
	done := start(t, options(t, srv.URL, t.TempDir()))
	select {
	case err := <-done:
		if !errors.Is(err, device.ErrUnsupportedVersion) || !strings.Contains(err.Error(), "plugin 0.9.0 speaks protocol 4 to 5") {
			t.Errorf("Run = %v", err)
		}
		done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("client kept running")
	}
}

func TestNoResumeAfterWelcome3Retries(t *testing.T) {
	t.Parallel()
	f := &fakeHub{skipResume: true, handler: func(ctx context.Context, _ *websocket.Conn, _ protocol.Hello) {
		<-ctx.Done()
	}}
	srv := serve(t, f)
	opts := options(t, srv.URL, t.TempDir())
	opts.HelloTimeout = 50 * time.Millisecond
	start(t, opts)
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
}

// TestPongConfirmsExit checks that an exit the plugin confirmed by a pong
// is forgotten, and one it did not is sent again on resume.
func TestPongConfirmsExit(t *testing.T) {
	t.Parallel()
	for _, answer := range []bool{true, false} {
		t.Run(strconv.FormatBool(answer), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			finished := make(chan struct{})
			f := &resumeHub{grace: 30}
			f.resumeFor = func(n int32) []protocol.ResumeCommand {
				if n == 2 {
					return []protocol.ResumeCommand{{ID: "1", Offset: 0}}
				}
				return nil
			}
			f.handle = func(ctx context.Context, n int32, conn *websocket.Conn) {
				if n == 1 {
					writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "echo hi; exit 4", Cwd: root, Timeout: 10})
					readUntilExit(ctx, t, conn, "1")
					for {
						typ, data, err := readMsg(ctx, conn)
						if err != nil {
							return
						}
						if typ == protocol.TypePing {
							if answer {
								var p protocol.Ping
								_ = json.Unmarshal(data, &p)
								writeJSON(ctx, conn, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
								time.Sleep(300 * time.Millisecond)
							}
							_ = conn.Close(websocket.StatusGoingAway, "hub shutting down")
							return
						}
					}
				}
				r := readUntilExit(ctx, t, conn, "1")
				if answer && (r.exit.Code != protocol.ExitLost || r.output != "") {
					t.Errorf("confirmed exit sent again: %+v", r)
				}
				if !answer && (r.exit.Code != 4 || r.output != "hi\n") {
					t.Errorf("unconfirmed exit not sent again: %+v", r)
				}
				close(finished)
				<-ctx.Done()
			}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			opts := options(t, srv.URL, root)
			opts.PingInterval = 50 * time.Millisecond
			start(t, opts)
			wait(t, finished)
		})
	}
}

func TestResumeBufferBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gate := filepath.Join(root, "gate")
	finished := make(chan struct{})
	f := &resumeHub{grace: 30}
	f.resumeFor = func(n int32) []protocol.ResumeCommand {
		if n == 2 {
			return []protocol.ResumeCommand{{ID: "1", Offset: 6}}
		}
		return nil
	}
	f.handle = func(ctx context.Context, n int32, conn *websocket.Conn) {
		if n == 1 {
			writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Timeout: 60, Cwd: root,
				Cmd: "echo first; while [ ! -e " + gate + " ]; do sleep 0.02; done; seq 1 200 | sed s/^/line/"})
			waitOutput(ctx, t, conn, "1", "first\n")
			_ = conn.Close(websocket.StatusGoingAway, "hub shutting down")
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Error(err)
			}
			return
		}
		defer close(finished)
		r := readResumed(ctx, t, conn, "1")
		if len(r.outputs) == 0 || r.outputs[0].Offset <= 6 || r.exit.Code != 0 {
			t.Fatalf("resumed = %+v", r)
		}
		var out string
		for _, o := range r.outputs {
			out += o.Data
		}
		if len(out) > 64 || !strings.HasSuffix(out, "line200\n") {
			t.Errorf("resumed output %q, want at most the 64-byte buffer ending in line200", out)
		}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	opts := options(t, srv.URL, root)
	opts.ResumeBuffer = 64
	// The command must finish while the leg is down, so only the bounded buffer comes back on resume.
	opts.MinBackoff = 2 * time.Second
	opts.MaxBackoff = 2 * time.Second
	start(t, opts)
	wait(t, finished)
}
