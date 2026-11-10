package acphost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

const (
	fakeEnv     = "ACPHOST_TEST_FAKE_HERMES"
	placeholder = "/opt/data/hermes/device-placeholder"
	backendID   = "backend-alice"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" && len(os.Args) == 2 && os.Args[1] == "acp" {
		fakeHermes()
		return
	}
	_ = os.Setenv(fakeEnv, "1")
	os.Exit(m.Run())
}

// fakeHermes stands in for `hermes acp`: it answers every request with the
// line it received, and has a few test methods.
func fakeHermes() {
	fmt.Fprintln(os.Stderr, "fake hermes started")
	out := bufio.NewWriter(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_, _ = out.Write(append(b, '\n'))
		_ = out.Flush()
	}
	hang := false
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cwd *string `json:"cwd"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(line), &m)
		switch m.Method {
		case "session/list":
			var rows []map[string]any
			for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
				rows = append(rows, map[string]any{"sessionId": id, "cwd": placeholder, "title": "t " + id, "updatedAt": "2026-11-10T00:00:00Z"})
			}
			reply(m.ID, map[string]any{"sessions": rows, "nextCursor": "s5", "receivedCwd": m.Params.Cwd})
		case "test/hang":
			signal.Ignore(syscall.SIGTERM)
			child := exec.Command("sleep", "300")
			_ = child.Start()
			hang = true
			reply(m.ID, map[string]any{"pid": os.Getpid(), "child": child.Process.Pid})
		case "test/notify":
			_, _ = out.WriteString("this is not json\n")
			_, _ = out.WriteString(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1"}}` + "\n")
			reply(m.ID, map[string]any{"pid": os.Getpid()})
		case "test/exit":
			reply(m.ID, map[string]any{})
			os.Exit(3)
		default:
			if len(m.ID) > 0 {
				reply(m.ID, map[string]any{"line": line})
			}
		}
	}
	if hang {
		select {}
	}
}

type harness struct {
	t    *testing.T
	priv ed25519.PrivateKey
	srv  *Server
	url  string
	logs *syncBuffer
	home string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, priv: priv, logs: &syncBuffer{}, home: t.TempDir()}
	opts := Options{
		Verifier:     assertion.NewVerifier([]ed25519.PublicKey{pub}),
		BackendID:    backendID,
		Hermes:       exe,
		Placeholder:  placeholder,
		BindingsFile: filepath.Join(h.home, "device", "bindings.json"),
		MaxProcesses: 4,
		PingInterval: 5 * time.Second,
		StopGrace:    200 * time.Millisecond,
		KillGrace:    200 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(h.logs, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	h.srv = New(opts)
	ts := httptest.NewServer(h.srv.Handler())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		h.srv.Close()
	})
	h.url = ts.URL
	return h
}

func (h *harness) claims() assertion.Claims {
	now := time.Now()
	return assertion.Claims{
		Aud: backendID, Leg: assertion.LegACP, Method: "GET", Path: protocol.ACPHostPath,
		Sub: "alice", DeviceID: "d1", DeviceName: "desktop",
		IssuedAt: now.Unix(), Expires: now.Add(30 * time.Second).Unix(),
	}
}

func (h *harness) sign(c assertion.Claims) string {
	h.t.Helper()
	s, err := assertion.Sign(h.priv, c)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) dialWith(header string) *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if header != "" {
		hdr.Set(assertion.Header, header)
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.url, "http")+protocol.ACPHostPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		h.t.Fatal(err)
	}
	conn.SetReadLimit(maxMessage)
	h.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func (h *harness) dial() *websocket.Conn {
	return h.dialWith(h.sign(h.claims()))
}

func send(t *testing.T, conn *websocket.Conn, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func recv(t *testing.T, conn *websocket.Conn) (reply, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var r reply
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("bad message %q: %v", data, err)
	}
	return r, data
}

// echoed sends msg and returns the params object hermes acp received.
func echoed(t *testing.T, conn *websocket.Conn, msg string) map[string]any {
	t.Helper()
	send(t, conn, msg)
	r, data := recv(t, conn)
	var res struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || res.Line == "" {
		t.Fatalf("no echo in %s", data)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Line), &got); err != nil {
		t.Fatalf("hermes got a line that is not JSON: %q", res.Line)
	}
	p, _ := got["params"].(map[string]any)
	return p
}

func closeStatus(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	resp, err := http.Get(h.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	_, other, _ := ed25519.GenerateKey(nil)
	signOther := func(c assertion.Claims) string {
		s, _ := assertion.Sign(other, c)
		return s
	}
	cases := map[string]func() string{
		"missing": func() string { return "" },
		"garbage": func() string { return "v1.nope" },
		"bad key": func() string { return signOther(h.claims()) },
		"wrong aud": func() string {
			c := h.claims()
			c.Aud = "backend-bob"
			return h.sign(c)
		},
		"wrong leg": func() string {
			c := h.claims()
			c.Leg = assertion.LegExec
			return h.sign(c)
		},
		"wrong path": func() string {
			c := h.claims()
			c.Path = protocol.PluginExecPath
			return h.sign(c)
		},
		"expired": func() string {
			c := h.claims()
			c.IssuedAt -= 120
			c.Expires -= 120
			return h.sign(c)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			conn := h.dialWith(mk())
			if got := closeStatus(t, conn); got != protocol.CloseAssertionRefused {
				t.Errorf("close %d, want %d", got, protocol.CloseAssertionRefused)
			}
		})
	}
	t.Run("replay", func(t *testing.T) {
		enc := h.sign(h.claims())
		first := h.dialWith(enc)
		send(t, first, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		recv(t, first)
		second := h.dialWith(enc)
		if got := closeStatus(t, second); got != protocol.CloseAssertionRefused {
			t.Errorf("close %d, want %d", got, protocol.CloseAssertionRefused)
		}
	})
}

func TestSessionRewrites(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()
	for _, method := range []string{"session/new", "session/load", "session/resume", "session/fork"} {
		p := echoed(t, conn, `{"jsonrpc":"2.0","id":"r1","method":"`+method+`","params":{"sessionId":"s1","cwd":"/home/alice/src/app",
			"mcpServers":[{"name":"x","command":"/bin/sh","args":[],"env":[]}],"mcp_servers":[{"name":"y"}]}}`)
		if p["cwd"] != placeholder {
			t.Errorf("%s: cwd %v", method, p["cwd"])
		}
		if s, ok := p["mcpServers"].([]any); !ok || len(s) != 0 {
			t.Errorf("%s: mcpServers %v", method, p["mcpServers"])
		}
		if _, ok := p["mcp_servers"]; ok {
			t.Errorf("%s: mcp_servers kept", method)
		}
		if p["sessionId"] != "s1" {
			t.Errorf("%s: sessionId %v", method, p["sessionId"])
		}
	}
	p := echoed(t, conn, `{"jsonrpc":"2.0","id":2,"method":"session/new"}`)
	if p["cwd"] != placeholder {
		t.Errorf("no params: cwd %v", p["cwd"])
	}
	p = echoed(t, conn, "{\"jsonrpc\":\"2.0\",\n \"id\":3,\n \"method\":\"session/set_mode\",\n \"params\":{\"sessionId\":\"s1\",\"modeId\":\"a\\nb\"}}")
	if p["modeId"] != "a\nb" {
		t.Errorf("set_mode params %v", p)
	}
	time.Sleep(100 * time.Millisecond)
	if logs := h.logs.String(); !strings.Contains(logs, "hermes acp: fake hermes started") || !strings.Contains(logs, "conn=") {
		t.Errorf("stderr not logged with a connection prefix:\n%s", logs)
	}
}

func TestPromptLinks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()
	uris := []string{
		"file:///etc/passwd", "FILE:///etc/passwd", "/etc/passwd", "relative.txt", "C:\\Windows\\x",
		" \tfile:///etc/passwd", "fi\nle:///etc/passwd", "file://localhost/etc/passwd", "https://example.com/a", "zed://x",
	}
	var blocks []string
	for _, u := range uris {
		b, _ := json.Marshal(map[string]string{"type": "resource_link", "uri": u, "name": "n"})
		blocks = append(blocks, string(b))
	}
	blocks = append(blocks, `{"type":"text","text":"hi"}`, `{"type":"resource","resource":{"uri":"file:///x","text":"inline"}}`,
		`{"type":"resource_link","uri":42}`)
	p := echoed(t, conn, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[`+strings.Join(blocks, ",")+`]}}`)
	got, _ := p["prompt"].([]any)
	if len(got) != len(blocks) {
		t.Fatalf("prompt has %d blocks, want %d", len(got), len(blocks))
	}
	kept := map[string]bool{"https://example.com/a": true, "zed://x": true}
	for i, u := range uris {
		blk := got[i].(map[string]any)
		if kept[u] {
			if blk["type"] != "resource_link" || blk["uri"] != u {
				t.Errorf("%q: changed to %v", u, blk)
			}
			continue
		}
		if blk["type"] != "text" || !strings.Contains(blk["text"].(string), "removed; the bridge inlines device files") {
			t.Errorf("%q: not replaced: %v", u, blk)
		}
	}
	if blk := got[len(uris)].(map[string]any); blk["type"] != "text" || blk["text"] != "hi" {
		t.Errorf("text block changed: %v", blk)
	}
	if blk := got[len(uris)+1].(map[string]any); blk["type"] != "resource" {
		t.Errorf("embedded resource changed: %v", blk)
	}
	if blk := got[len(uris)+2].(map[string]any); blk["type"] != "text" {
		t.Errorf("link with a non-string uri kept: %v", blk)
	}
}

func TestInvalidMessages(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()
	for msg, code := range map[string]int{
		`not json`: codeParse,
		`[{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/home/alice"}}]`: codeInvalidReq,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":["/home/alice",[]]}`:      codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{"cwd":"rel"}}`:          codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":"x"}}`:       codeInvalidParams,
	} {
		send(t, conn, msg)
		r, data := recv(t, conn)
		if r.Error == nil || r.Error.Code != code {
			t.Errorf("%s: got %s, want code %d", msg, data, code)
		}
	}
	send(t, conn, `{"jsonrpc":"2.0","id":9,"method":"test/notify"}`)
	r, data := recv(t, conn)
	if r.Method != "session/update" {
		t.Errorf("non-JSON stdout line not dropped: %s", data)
	}
	recv(t, conn)
}

func writeBindings(t *testing.T, h *harness) {
	t.Helper()
	data := `{"version":2,"bindings":{
		"s1":{"device_id":"d1","dir":"/home/alice/src/app","generation":"g1","bound_at":1794300000.5},
		"s2":{"device_id":"d1","dir":"/home/alice/src/app/sub","generation":"g2","bound_at":1794300000.5},
		"s3":{"device_id":"d1","dir":"/home/alice/other","generation":"g3","bound_at":1794300000.5},
		"s4":{"device_id":"d2","dir":"/home/alice/src/app","generation":"g4","bound_at":1794300000.5}},
		"ever_bound":["s1","s2","s3","s4","s5"]}`
	if err := os.MkdirAll(filepath.Dir(h.srv.opts.BindingsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.srv.opts.BindingsFile, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

type listResult struct {
	Sessions []struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Title     string `json:"title"`
	} `json:"sessions"`
	NextCursor  string  `json:"nextCursor"`
	ReceivedCwd *string `json:"receivedCwd"`
}

func list(t *testing.T, conn *websocket.Conn, params string) listResult {
	t.Helper()
	send(t, conn, `{"jsonrpc":"2.0","id":"list-1","method":"session/list","params":`+params+`}`)
	r, data := recv(t, conn)
	var res listResult
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatalf("bad list result %s", data)
	}
	return res
}

func rows(res listResult) []string {
	var out []string
	for _, s := range res.Sessions {
		out = append(out, s.SessionID+"="+s.Cwd)
	}
	return out
}

func TestListFiltering(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()

	if res := list(t, conn, `{}`); len(res.Sessions) != 0 || res.NextCursor != "s5" {
		t.Errorf("no bindings file: %v, cursor %q", rows(res), res.NextCursor)
	}

	writeBindings(t, h)
	res := list(t, conn, `{"cwd":"/home/alice/src"}`)
	if want := []string{"s1=/home/alice/src/app", "s2=/home/alice/src/app/sub"}; !slices.Equal(rows(res), want) {
		t.Errorf("cwd filter: %v, want %v", rows(res), want)
	}
	if res.ReceivedCwd == nil || *res.ReceivedCwd != placeholder {
		t.Errorf("hermes got cwd %v", res.ReceivedCwd)
	}
	if res.NextCursor != "s5" {
		t.Errorf("cursor %q", res.NextCursor)
	}
	res = list(t, conn, `{"cwd":"/home/alice/src/app/"}`)
	if want := []string{"s1=/home/alice/src/app", "s2=/home/alice/src/app/sub"}; !slices.Equal(rows(res), want) {
		t.Errorf("exact dir: %v, want %v", rows(res), want)
	}
	res = list(t, conn, `{"cwd":"/home/alice/src/ap"}`)
	if len(res.Sessions) != 0 {
		t.Errorf("prefix that is not a parent matched: %v", rows(res))
	}
	res = list(t, conn, `{"cursor":"s0"}`)
	if want := []string{"s1=/home/alice/src/app", "s2=/home/alice/src/app/sub", "s3=/home/alice/other"}; !slices.Equal(rows(res), want) {
		t.Errorf("no cwd: %v, want %v", rows(res), want)
	}
	if res.ReceivedCwd != nil {
		t.Errorf("hermes got a cwd for an unfiltered list: %v", *res.ReceivedCwd)
	}

	c := h.claims()
	c.DeviceID = "d2"
	other := h.dialWith(h.sign(c))
	if res := list(t, other, `{}`); !slices.Equal(rows(res), []string{"s4=/home/alice/src/app"}) {
		t.Errorf("device d2: %v", rows(res))
	}

	if err := os.WriteFile(h.srv.opts.BindingsFile, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := list(t, conn, `{}`); len(res.Sessions) != 0 {
		t.Errorf("broken bindings file listed %v", rows(res))
	}
}

func TestProcessLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.MaxProcesses = 1 })
	first := h.dial()
	send(t, first, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	recv(t, first)
	second := h.dial()
	if got := closeStatus(t, second); got != CloseTryAgainLater {
		t.Errorf("close %d, want 1013", got)
	}
	_ = first.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		third := h.dial()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := third.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		var rerr error
		if err == nil {
			_, _, rerr = third.Read(ctx)
		}
		cancel()
		if err == nil && rerr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed after the first connection closed: %v %v", err, rerr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func gone(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	data, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if rerr != nil {
		return true
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] == "Z"
}

func waitGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range pids {
		for !gone(pid) {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("process %d still running", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestChildCleanup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	conn := h.dial()
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"test/notify"}`)
	recv(t, conn)
	r, _ := recv(t, conn)
	var plain struct{ Pid int }
	_ = json.Unmarshal(r.Result, &plain)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitGone(t, plain.Pid)

	conn = h.dial()
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"test/hang"}`)
	r, _ = recv(t, conn)
	var hung struct{ Pid, Child int }
	if err := json.Unmarshal(r.Result, &hung); err != nil || hung.Pid == 0 || hung.Child == 0 {
		t.Fatalf("bad hang reply %s", r.Result)
	}
	start := time.Now()
	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitGone(t, hung.Pid, hung.Child)
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Errorf("child that ignores SIGTERM ended after %v, before the grace periods", el)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "sending SIGKILL") {
		t.Errorf("no SIGKILL logged:\n%s", logs)
	}
}

func TestChildExitClosesLeg(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"test/exit"}`)
	recv(t, conn)
	if got := closeStatus(t, conn); got != CloseChildExited {
		t.Errorf("close %d, want %d", got, CloseChildExited)
	}
}

func TestReadsLocalPath(t *testing.T) {
	t.Parallel()
	for uri, want := range map[string]bool{
		"file:///a": true, "File:/a": true, "/a": true, "a": true, "c:/a": true, "D:\\a": true, "": true,
		"\x01file:///a": true, "ht tp://x": true, "\u00a0https://x": true,
		"https://x": false, "zed:x": false, "git+ssh://x": false,
	} {
		if got := readsLocalPath(uri); got != want {
			t.Errorf("readsLocalPath(%q) = %v, want %v", uri, got, want)
		}
	}
}
