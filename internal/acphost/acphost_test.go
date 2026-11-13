package acphost_test

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
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/assertion"
	"github.com/USA-RedDragon/talaria/internal/acphost"
	"github.com/USA-RedDragon/talaria/protocol"
	"github.com/coder/websocket"
)

const (
	fakeEnv     = "ACPHOST_TEST_FAKE_HERMES"
	placeholder = "/opt/data/hermes/device-placeholder"
	backendID   = "backend-alice"

	// readLimit matches the largest stdout line acp-host copies.
	readLimit = 64 << 20
)

// JSON-RPC 2.0 error codes.
const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeInvalidParams = -32602
	codeInternal      = -32603
)

const (
	keyPID       = "pid"
	methodLoad   = "session/load"
	methodResume = "session/resume"
	blockText    = "text"
	rowApp       = "s1=/home/alice/src/app"
	rowAppSub    = "s2=/home/alice/src/app/sub"
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
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake hermes: encode reply:", err)
			os.Exit(2)
		}
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
			ids := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
			rows := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				rows = append(rows, map[string]any{"sessionId": id, "cwd": placeholder, "title": "t " + id, "updatedAt": "2026-11-10T00:00:00Z"})
			}
			reply(m.ID, map[string]any{"sessions": rows, "nextCursor": "s5", "receivedCwd": m.Params.Cwd})
		case "test/hang":
			signal.Ignore(syscall.SIGTERM)
			child := exec.CommandContext(context.Background(), "sleep", "300")
			_ = child.Start()
			hang = true
			reply(m.ID, map[string]any{keyPID: os.Getpid(), "child": child.Process.Pid})
		case "test/notify":
			_, _ = out.WriteString("this is not json\n")
			_, _ = out.WriteString(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1"}}` + "\n")
			reply(m.ID, map[string]any{keyPID: os.Getpid()})
		case "test/detach":
			child := exec.CommandContext(context.Background(), "sleep", "300")
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			_ = child.Start()
			reply(m.ID, map[string]any{keyPID: os.Getpid(), "child": child.Process.Pid})
		case "test/stderr":
			_, _ = os.Stderr.WriteString(strings.Repeat("x", 2<<20) + "\n")
			_, _ = os.Stderr.WriteString(strings.Repeat("y", 4<<20) + "\n")
			reply(m.ID, map[string]any{})
		case methodLoad, methodResume:
			reply(m.ID, map[string]any{"line": line, "_meta": map[string]any{"provenance": "p"}})
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
		// A sleeping goroutine, not select {}: with no other goroutine, the runtime
		// would end a blocked select as a deadlock instead of waiting for SIGKILL.
		for {
			time.Sleep(time.Hour)
		}
	}
}

type harness struct {
	t        *testing.T
	priv     ed25519.PrivateKey
	srv      *acphost.Server
	url      string
	logs     *syncBuffer
	home     string
	bindings string
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

func newHarness(t *testing.T, tweak func(*acphost.Options)) *harness {
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
	h.bindings = filepath.Join(h.home, "device", "bindings.json")
	opts := acphost.Options{
		Verifier:     assertion.NewVerifier([]ed25519.PublicKey{pub}),
		BackendID:    backendID,
		Hermes:       exe,
		Placeholder:  placeholder,
		BindingsFile: h.bindings,
		MaxProcesses: 4,
		PingInterval: 5 * time.Second,
		StopGrace:    200 * time.Millisecond,
		KillGrace:    200 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(h.logs, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	h.srv = acphost.New(opts)
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
	conn.SetReadLimit(readLimit)
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
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
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
			t.Parallel()
			conn := h.dialWith(mk())
			if got := closeStatus(t, conn); got != protocol.CloseAssertionRefused {
				t.Errorf("close %d, want %d", got, protocol.CloseAssertionRefused)
			}
		})
	}
	t.Run("replay", func(t *testing.T) {
		t.Parallel()
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
	writeBindings(t, h)
	conn := h.dial()
	for _, method := range []string{"session/new", methodLoad, methodResume, "session/fork"} {
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
		want := any("s1")
		if method == "session/new" {
			want = nil
		}
		if p["sessionId"] != want {
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
	writeBindings(t, h)
	conn := h.dial()
	uris := []string{
		"file:///etc/passwd", "FILE:///etc/passwd", "/etc/passwd", "relative.txt", "C:\\Windows\\x",
		" \tfile:///etc/passwd", "fi\nle:///etc/passwd", "file://localhost/etc/passwd", "https://example.com/a", "zed://x",
	}
	blocks := make([]string, 0, len(uris)+3)
	for _, u := range uris {
		b, err := json.Marshal(map[string]string{"type": "resource_link", "uri": u, "name": "n"})
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, string(b))
	}
	blocks = append(blocks, `{"type":"text","text":"hi"}`, `{"type":"resource","resource":{"uri":"file:///x","text":"inline"}}`,
		`{"type":"resource_link","uri":42}`)
	p := echoed(t, conn, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[`+strings.Join(blocks, ",")+`]}}`)
	got, _ := p["prompt"].([]any)
	if len(got) != len(blocks) {
		t.Fatalf("prompt has %d blocks, want %d", len(got), len(blocks))
	}
	kept := map[string]bool{}
	for i, u := range uris {
		blk := asMap(t, got[i])
		if kept[u] {
			if blk["type"] != "resource_link" || blk["uri"] != u {
				t.Errorf("%q: changed to %v", u, blk)
			}
			continue
		}
		if blk["type"] != blockText || blk[blockText] != "[talaria: link "+u+" removed; the bridge inlines device files]" {
			t.Errorf("%q: not replaced: %v", u, blk)
		}
	}
	if blk := asMap(t, got[len(uris)]); blk["type"] != blockText || blk[blockText] != "hi" {
		t.Errorf("text block changed: %v", blk)
	}
	if blk := asMap(t, got[len(uris)+1]); blk["type"] != "resource" {
		t.Errorf("embedded resource changed: %v", blk)
	}
	if blk := asMap(t, got[len(uris)+2]); blk["type"] != blockText {
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
	if err := os.MkdirAll(filepath.Dir(h.bindings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.bindings, []byte(data), 0o600); err != nil {
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
	out := make([]string, 0, len(res.Sessions))
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
	res := list(t, conn, `{"cwd":"/home/alice/src"}`)
	if len(res.Sessions) != 6 || res.Sessions[5].Cwd != "/home/alice/src" {
		t.Errorf("no bindings file, never-bound rows in the folder: %v", rows(res))
	}

	writeBindings(t, h)
	res = list(t, conn, `{"cwd":"/home/alice/src"}`)
	if want := []string{rowApp, rowAppSub, "s6=/home/alice/src"}; !slices.Equal(rows(res), want) {
		t.Errorf("cwd filter: %v, want %v", rows(res), want)
	}
	if res.ReceivedCwd != nil {
		t.Errorf("hermes got cwd %v", *res.ReceivedCwd)
	}
	if res.NextCursor != "s5" {
		t.Errorf("cursor %q", res.NextCursor)
	}
	res = list(t, conn, `{"cwd":"/home/alice/src/app/"}`)
	if want := []string{rowApp, rowAppSub, "s6=/home/alice/src/app"}; !slices.Equal(rows(res), want) {
		t.Errorf("exact dir: %v, want %v", rows(res), want)
	}
	res = list(t, conn, `{"cwd":"/home/alice/src/ap"}`)
	if want := []string{"s6=/home/alice/src/ap"}; !slices.Equal(rows(res), want) {
		t.Errorf("prefix that is not a parent: %v, want %v", rows(res), want)
	}
	res = list(t, conn, `{"cursor":"s0"}`)
	if want := []string{rowApp, rowAppSub, "s3=/home/alice/other"}; !slices.Equal(rows(res), want) {
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
	if res := list(t, other, `{"cwd":"/home/bob"}`); !slices.Equal(rows(res), []string{"s6=/home/bob"}) {
		t.Errorf("device d2 in another folder: %v", rows(res))
	}

	if err := os.WriteFile(h.bindings, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, params := range []string{`{}`, `{"cwd":"/home/alice/src"}`} {
		if res := list(t, conn, params); len(res.Sessions) != 0 {
			t.Errorf("broken bindings file listed %v for %s", rows(res), params)
		}
	}
}

// TestLoadCarriesBinding checks that a session/load or session/resume
// result carries the session's own binding to the connection's device, and
// keeps Hermes' own _meta and the editor's id.
func TestLoadCarriesBinding(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	writeBindings(t, h)
	conn := h.dial()
	for _, tc := range []struct{ method, id, sid, dir string }{
		{methodLoad, `"l1"`, "s1", "/home/alice/src/app"},
		{methodLoad, `1e0`, "s3", "/home/alice/other"},
		{methodResume, `null`, "s2", "/home/alice/src/app/sub"},
	} {
		send(t, conn, `{"jsonrpc":"2.0","id":`+tc.id+`,"method":"`+tc.method+`","params":{"sessionId":"`+tc.sid+`","cwd":"/home/bob/elsewhere","mcpServers":[]}}`)
		_, data := recv(t, conn)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s %s: %s", tc.method, tc.sid, data)
		}
		if string(m.ID) != tc.id {
			t.Errorf("%s: id %s came back as %s", tc.method, tc.id, m.ID)
		}
		var b struct{ Dir string }
		_ = json.Unmarshal(m.Result.Meta[protocol.ACPBindingMeta], &b)
		if b.Dir != tc.dir || string(m.Result.Meta["provenance"]) != `"p"` {
			t.Errorf("%s %s: _meta %s", tc.method, tc.sid, data)
		}
	}

	if err := os.WriteFile(h.bindings, []byte(`{"bindings":{"s1":{"device_id":"d1","dir":"/home/alice/src/app"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	send(t, conn, `{"jsonrpc":"2.0","id":9,"method":"session/load","params":{"sessionId":"s1","cwd":"/home/alice/src/app","mcpServers":[]}}`)
	_, data := recv(t, conn)
	if !strings.Contains(string(data), `"`+protocol.ACPBindingMeta+`":{"dir":"/home/alice/src/app"}`) {
		t.Errorf("load got %s", data)
	}
	send(t, conn, `{"jsonrpc":"2.0","id":10,"method":"session/new","params":{"cwd":"/home/alice/src/app","mcpServers":[]}}`)
	if _, data := recv(t, conn); strings.Contains(string(data), protocol.ACPBindingMeta) {
		t.Errorf("session/new got a binding: %s", data)
	}
}

func TestProcessLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *acphost.Options) { o.MaxProcesses = 1 })
	first := h.dial()
	send(t, first, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	recv(t, first)
	second := h.dial()
	if got := closeStatus(t, second); got != acphost.CloseTryAgainLater {
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
	if runtime.GOOS != "linux" {
		// No /proc to tell a zombie: Kill found the process
		return false
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
	if got := closeStatus(t, conn); got != acphost.CloseChildExited {
		t.Errorf("close %d, want %d", got, acphost.CloseChildExited)
	}
}

func TestOtherDeviceRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	writeBindings(t, h)
	conn := h.dial()
	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"s4","cwd":"/x","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"s4","cwd":"/x","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/fork","params":{"sessionId":"s4","cwd":"/x","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s4","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"session_id":"s4","prompt":[]}}`,
	} {
		send(t, conn, msg)
		r, data := recv(t, conn)
		if r.Error == nil || r.Error.Code != codeInvalidParams || r.Error.Message != "session is bound to another device" {
			t.Errorf("%s: got %s", msg, data)
		}
	}
	for _, sid := range []string{"s5", "unknown", "s1-child"} {
		for _, method := range []string{methodLoad, methodResume, "session/fork", "session/prompt", "session/set_mode", "session/close"} {
			send(t, conn, `{"jsonrpc":"2.0","id":2,"method":"`+method+`","params":{"sessionId":"`+sid+`","cwd":"/x","mcpServers":[],"prompt":[]}}`)
			r, data := recv(t, conn)
			if r.Error == nil || r.Error.Code != codeInvalidParams || r.Error.Message != "session is not bound to this device" {
				t.Errorf("%s %s: got %s", method, sid, data)
			}
		}
	}
	p := echoed(t, conn, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"session_id":"s1","prompt":[]}}`)
	if p["sessionId"] != "s1" || p["session_id"] != nil {
		t.Errorf("s1: params %v", p)
	}
	if err := os.WriteFile(h.bindings, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	send(t, conn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s1","prompt":[]}}`)
	if r, data := recv(t, conn); r.Error == nil || r.Error.Code != codeInternal {
		t.Errorf("broken bindings file: got %s", data)
	}
}

// TestParamsPinned checks that no _meta member reaches the ACP SDK, which
// merges params._meta (or params.field_meta) into the handler's keyword
// arguments, so {"_meta":{"session_id":...}} would replace the session id
// acp-host checked and {"_meta":{"cwd":...}} the placeholder.
func TestParamsPinned(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	writeBindings(t, h)
	conn := h.dial()
	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}],"_meta":{"session_id":"s4"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[],"field_meta":{"session_id":"s4"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[],"\u005fmeta":{"session_id":"s4"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/home/alice","mcpServers":[],"_meta":{"cwd":"/opt/data"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"s1","cwd":"/home/alice","mcpServers":[],"_meta":{"cwd":"/opt/data","session_id":"s4"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/set_config_option","params":{"sessionId":"s1","configId":"c","value":"v","_meta":{"session_id":"s4"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"_meta":{"protocol_version":2}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"other/method","params":{"a":1,"_meta":{"session_id":"s4"},"field_meta":{}}}`,
	} {
		p := echoed(t, conn, msg)
		for _, k := range []string{"_meta", "field_meta"} {
			if _, ok := p[k]; ok {
				t.Errorf("%s: %s reached hermes: %v", msg, k, p)
			}
		}
		if cwd, ok := p["cwd"]; ok && cwd != placeholder {
			t.Errorf("%s: cwd %v", msg, cwd)
		}
		if sid, ok := p["sessionId"]; ok && sid != "s1" {
			t.Errorf("%s: sessionId %v", msg, sid)
		}
	}

	p := echoed(t, conn, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"session_id":"s4","sessionId":"s1",
		"prompt":[{"type":"text","text":"hi","_meta":{"k":"v"}}],"messageId":"m","extra":1}}`)
	if p["sessionId"] != "s1" || p["session_id"] != nil || p["extra"] != nil || p["messageId"] != "m" {
		t.Errorf("prompt params not pinned: %v", p)
	}
	blocks, _ := p["prompt"].([]any)
	if len(blocks) != 1 || asMap(t, blocks[0])["_meta"] == nil {
		t.Errorf("a content block lost its _meta, which the SDK does not merge: %v", p["prompt"])
	}
	p = echoed(t, conn, `{"jsonrpc":"2.0","id":3,"method":"session/set_mode","params":{"session_id":"s1","mode_id":"m"}}`)
	if p["sessionId"] != "s1" || p["modeId"] != "m" || len(p) != 2 {
		t.Errorf("field names not folded into aliases: %v", p)
	}
	p = echoed(t, conn, `{"jsonrpc":"2.0","id":4,"method":"other/method","params":[1]}`)
	if p != nil {
		t.Errorf("array params of an unknown method changed: %v", p)
	}

	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":5,"method":"session/set_config_option","params":{"sessionId":"s4","configId":"c","value":"v"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"session/set_mode","params":{"session_id":"s4","modeId":"m"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"session/set_model","params":{"sessionId":"s4","modelId":"m"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"session/close","params":{"sessionId":"s4"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"other/method","params":{"session_id":"s4"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":["s1",[]]}`,
	} {
		send(t, conn, msg)
		r, data := recv(t, conn)
		if r.Error == nil || r.Error.Code != codeInvalidParams {
			t.Errorf("%s: got %s", msg, data)
		}
	}
	send(t, conn, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s4"}}`)
	echoed(t, conn, `{"jsonrpc":"2.0","id":6,"method":"initialize","params":{}}`)
}

// TestSlashCommands checks that a prompt reaches only the slash commands
// the gateway leg allows: hermes acp sends other commands to slash.exec,
// and /device there would move the session to another device.
func TestSlashCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	writeBindings(t, h)
	conn := h.dial()
	prompt := func(texts ...string) string {
		blocks := make([]map[string]string, 0, len(texts))
		for _, x := range texts {
			blocks = append(blocks, map[string]string{"type": blockText, blockText: x})
		}
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/prompt",
			"params": map[string]any{"sessionId": "s1", "prompt": blocks}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, texts := range [][]string{
		{"  /new"}, {"/reasoning high"}, {"/reasoning"}, {"/Compress"}, {"/discard a1"}, {"/reset"}, {"/compact"},
		{"/etc/hosts is wrong"}, {"hello /device d2 /"}, {"/"}, {"", "plain"}, {"text", "/device d2 /"}, {"/usage"},
		{"/new@bot"}, {"@notes.md\n/new"},
	} {
		echoed(t, conn, prompt(texts...))
	}
	for _, texts := range [][]string{
		{"/device d2 /home/alice"}, {"/DEVICE d2 /"}, {"\u001c\t/device d2 /"}, {"/device"}, {" ", "/device d2 /"},
		{"", "/plugins install x"}, {"/reasoning show"}, {"/model gpt-x"}, {"/\uff44evice d2 /"}, {"/verbose"},
		{"/diff"}, {"/refine"}, {"/usage reset"}, {"/fast on '--GLOBAL'"}, {"/yolo \u2014global"}, {"/device@bot d2 /"},
		{"@notes.md\n/device d2 /"}, {"/model x --global"},
	} {
		send(t, conn, prompt(texts...))
		r, data := recv(t, conn)
		if r.Error == nil || r.Error.Code != codeInvalidParams || !strings.Contains(r.Error.Message, "is not available through talaria") {
			t.Errorf("%q: got %s", texts, data)
		}
	}
	send(t, conn, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"resource","resource":{"uri":"file:///x","text":"/device d2 /"}},{"type":"text","text":"/device d2 /"}]}}`)
	if r, data := recv(t, conn); r.Error == nil {
		t.Errorf("command after a resource block: got %s", data)
	}
}

func TestGatewayHealthGate(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	status := http.StatusServiceUnavailable
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(gw.Close)
	h := newHarness(t, func(o *acphost.Options) { o.GatewayHealthURL = gw.URL + "/api/health" })
	if got := closeStatus(t, h.dial()); got != acphost.CloseTryAgainLater {
		t.Errorf("gateway down: close %d, want 1013", got)
	}
	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	conn := h.dial()
	echoed(t, conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
}

func TestSweepStopsDetached(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the sweep of detached processes needs Linux: a subreaper and /proc")
	}
	h := newHarness(t, func(o *acphost.Options) { o.SweepInterval = 50 * time.Millisecond })
	conn := h.dial()
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"test/detach"}`)
	r, _ := recv(t, conn)
	var got struct{ Pid, Child int }
	if err := json.Unmarshal(r.Result, &got); err != nil || got.Child == 0 {
		t.Fatalf("bad reply %s", r.Result)
	}
	waitGone(t, got.Child)
	if gone(got.Pid) {
		t.Error("the sweep stopped hermes acp itself")
	}
}

func TestListIDs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	writeBindings(t, h)
	conn := h.dial()
	for _, id := range []string{`null`, `1e0`, `"\u00e9"`, `"hermes-device-list-1"`, `-7`} {
		send(t, conn, `{"jsonrpc":"2.0","id":`+id+`,"method":"session/list","params":{}}`)
		_, data := recv(t, conn)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(data, &m)
		if string(m["id"]) != id {
			t.Errorf("id %s came back as %s", id, m["id"])
		}
		var res listResult
		_ = json.Unmarshal(m["result"], &res)
		if len(res.Sessions) != 3 {
			t.Errorf("id %s: unfiltered list %v", id, rows(res))
		}
	}
}

func TestNotificationsGetNoAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()
	send(t, conn, `{"jsonrpc":"2.0","method":"session/prompt","params":{"prompt":"x"}}`)
	send(t, conn, `{"jsonrpc":"2.0","method":"session/new","params":[]}`)
	p := echoed(t, conn, `{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":1}}`)
	if p["protocolVersion"] != float64(1) {
		t.Errorf("params %v", p)
	}
}

func TestLongStderrLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.dial()
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"test/stderr"}`)
	recv(t, conn)
	echoed(t, conn, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`)
}

// asMap returns v as a JSON object, failing the test when it is not one.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v (%T) is not a JSON object", v, v)
	}
	return m
}
