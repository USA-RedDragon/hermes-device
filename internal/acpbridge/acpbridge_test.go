package acpbridge_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/acpbridge"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

const (
	jsonRPC            = "2.0"
	methodInitialize   = "initialize"
	methodUpdate       = "session/update"
	keyProtocolVersion = "protocolVersion"
	keyJSONRPC         = "jsonrpc"
	keySessionID       = "sessionId"
	localSession       = "local-1"
)

type fakeHub struct {
	t     *testing.T
	ts    *httptest.Server
	conns chan *websocket.Conn
	stop  chan struct{}

	mu       sync.Mutex
	binds    []string
	failBind map[string]int
	refuse   bool
	status   int
	refused  int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{t: t, conns: make(chan *websocket.Conn, 4), stop: make(chan struct{}), failBind: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+hubapi.PathACPLeg, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		refuse, code := h.refuse, h.status
		if refuse {
			h.refused++
		}
		h.mu.Unlock()
		if refuse || r.Header.Get("Authorization") != "Bearer hdt_test" {
			if code == 0 {
				code = http.StatusUnauthorized
			}
			w.WriteHeader(code)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(64 << 20)
		h.conns <- conn
		<-h.stop
	})
	mux.HandleFunc("PUT /api/device/sessions/{id}/binding", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Dir string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		status := h.failBind[body.Dir]
		h.binds = append(h.binds, r.PathValue("id")+"="+body.Dir)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"device_offline","message":"device desktop is not connected"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session_id": r.PathValue("id"), "device_id": "d1", "dir": body.Dir})
	})
	h.ts = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(h.stop)
		h.ts.CloseClientConnections()
		h.ts.Close()
	})
	return h
}

// setRefuse makes the hub answer every upgrade with status (0 accepts
// again) and returns how many upgrades it refused so far.
func (h *fakeHub) setRefuse(status int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuse, h.status = status != 0, status
	return h.refused
}

func (h *fakeHub) refusedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refused
}

func (h *fakeHub) bindLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.binds...)
}

func (h *fakeHub) accept() *agent {
	h.t.Helper()
	select {
	case c := <-h.conns:
		h.t.Cleanup(func() { _ = c.CloseNow() })
		return &agent{t: h.t, conn: c}
	case <-time.After(10 * time.Second):
		h.t.Fatal("bridge did not connect")
		return nil
	}
}

// agent is the test's side of one ACP leg, acting as hermes acp.
type agent struct {
	t    *testing.T
	conn *websocket.Conn
}

func (a *agent) recv() map[string]any {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := a.conn.Read(ctx)
	if err != nil {
		a.t.Fatalf("agent read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		a.t.Fatalf("agent got %q: %v", data, err)
	}
	return m
}

func (a *agent) send(msg any) {
	a.t.Helper()
	data, ok := msg.(string)
	if !ok {
		b, err := json.Marshal(msg)
		if err != nil {
			a.t.Fatalf("agent encode: %v", err)
		}
		data = string(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.conn.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
		a.t.Fatalf("agent write: %v", err)
	}
}

func (a *agent) answer(req map[string]any, result any) {
	a.t.Helper()
	a.send(map[string]any{keyJSONRPC: jsonRPC, "id": req["id"], "result": result})
}

type editor struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan edLine
	done  chan error
}

type edLine struct {
	m   map[string]any
	raw []byte
}

func startBridge(t *testing.T, hub *fakeHub) *editor {
	t.Helper()
	return startBridgeReady(t, hub, 300*time.Millisecond)
}

func startBridgeReady(t *testing.T, hub *fakeHub, readyTimeout time.Duration) *editor {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	e := &editor{t: t, in: inW, lines: make(chan edLine, 64), done: make(chan error, 1)}
	br := acpbridge.New(acpbridge.Options{
		Hub: hubapi.New(hub.ts.URL, "hdt_test", "test"), In: inR, Out: outW,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, PingInterval: 5 * time.Second,
		ReadyTimeout: readyTimeout,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("bridge wrote a line that is not JSON: %q", sc.Text())
				continue
			}
			e.lines <- edLine{m: m, raw: append([]byte(nil), sc.Bytes()...)}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		e.done <- br.Run(ctx)
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		cancel()
		select {
		case err := <-e.done:
			if err != nil {
				t.Logf("bridge returned after cancel: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	return e
}

func (e *editor) send(msg string) {
	e.t.Helper()
	if _, err := io.WriteString(e.in, msg+"\n"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *editor) recv() map[string]any {
	e.t.Helper()
	return e.recvLine().m
}

func (e *editor) recvLine() edLine {
	e.t.Helper()
	select {
	case l := <-e.lines:
		return l
	case <-time.After(10 * time.Second):
		e.t.Fatal("editor got nothing")
		return edLine{}
	}
}

func (e *editor) none() {
	e.t.Helper()
	select {
	case l := <-e.lines:
		e.t.Fatalf("editor got unexpected %s", l.raw)
	case <-time.After(100 * time.Millisecond):
	}
}

func errCode(m map[string]any) (int, string) {
	e, _ := m["error"].(map[string]any)
	if e == nil {
		return 0, ""
	}
	code, _ := e["code"].(float64)
	msg, _ := e["message"].(string)
	return int(code), msg
}

func param(m map[string]any, key string) any {
	p, _ := m["params"].(map[string]any)
	return p[key]
}

func TestSessionNewBindsAndIDs(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()

	ed.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	req := a.recv()
	if id, _ := req["id"].(string); !strings.HasPrefix(id, acpbridge.IDPrefix) || req["method"] != methodInitialize {
		t.Fatalf("agent got %v", req)
	}
	a.answer(req, map[string]any{keyProtocolVersion: 1})
	if r := ed.recv(); r["id"] != float64(1) || r["result"] == nil {
		t.Fatalf("editor got %v", r)
	}

	ed.send(`{"jsonrpc":"2.0","id":"n1","method":"session/new","params":{"cwd":"/home/alice/src/app","mcpServers":[]}}`)
	req = a.recv()
	if param(req, "cwd") != "/home/alice/src/app" {
		t.Errorf("cwd %v", param(req, "cwd"))
	}
	if len(hub.bindLog()) != 0 {
		t.Error("bound before the result")
	}
	a.answer(req, map[string]any{keySessionID: localSession})
	r := ed.recv()
	if r["id"] != "n1" || asMap(t, r["result"])[keySessionID] != localSession {
		t.Fatalf("editor got %v", r)
	}
	if got := hub.bindLog(); len(got) != 1 || got[0] != "local-1=/home/alice/src/app" {
		t.Errorf("binds %v", got)
	}

	a.send(`{"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"local-1"}}`)
	r = ed.recv()
	pid, _ := r["id"].(string)
	if !strings.HasPrefix(pid, acpbridge.IDPrefix) || r["method"] != "session/request_permission" {
		t.Fatalf("editor got %v", r)
	}
	ed.send(`{"jsonrpc":"2.0","id":"` + pid + `","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`)
	if got := a.recv(); got["id"] != float64(0) || got["result"] == nil {
		t.Errorf("agent got %v", got)
	}

	ed.send(`{"jsonrpc":"2.0","id":"f1","method":"session/fork","params":{"sessionId":"local-1","cwd":"/home/alice/src/app/sub/","mcpServers":[]}}`)
	a.answer(a.recv(), map[string]any{keySessionID: "local-2"})
	if r := ed.recv(); r["id"] != "f1" || r["error"] != nil {
		t.Fatalf("editor got %v", r)
	}
	if got := hub.bindLog(); len(got) != 2 || got[1] != "local-2=/home/alice/src/app/sub" {
		t.Errorf("binds %v", got)
	}

	a.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-1","update":{}}}`)
	if r := ed.recv(); r["method"] != methodUpdate {
		t.Errorf("editor got %v", r)
	}

	ed.send(`{not json`)
	if code, _ := errCode(ed.recv()); code != acpbridge.CodeParse {
		t.Errorf("parse error code %d", code)
	}
	ed.send(`{"jsonrpc":"2.0","id":7,"method":"session/new","params":{"cwd":"rel","mcpServers":[]}}`)
	if code, _ := errCode(ed.recv()); code != acpbridge.CodeInvalidParams {
		t.Errorf("relative cwd code %d", code)
	}
}

func TestBindFailures(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	hub.failBind["/home/alice/offline"] = http.StatusConflict
	ed := startBridge(t, hub)
	a := hub.accept()

	ed.send(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/home/alice/offline","mcpServers":[]}}`)
	a.answer(a.recv(), map[string]any{keySessionID: "local-9"})
	r := ed.recv()
	code, msg := errCode(r)
	if r["id"] != float64(1) || code != acpbridge.CodeInternal || !strings.HasPrefix(msg, "hermes-device: could not bind the session to /home/alice/offline: ") ||
		!strings.Contains(msg, "device_offline") {
		t.Errorf("editor got %v", r)
	}

	ed.send(`{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"local-3","cwd":"/home/alice/offline","mcpServers":[]}}`)
	r = ed.recv()
	if code, _ := errCode(r); r["id"] != float64(2) || code != acpbridge.CodeInternal {
		t.Errorf("editor got %v", r)
	}
	ed.send(`{"jsonrpc":"2.0","id":3,"method":"session/resume","params":{"sessionId":"local-3","cwd":"/home/alice/src","mcpServers":[]}}`)
	req := a.recv()
	if req["method"] != "session/resume" {
		t.Fatalf("a failed load was forwarded: %v", req)
	}
	if got := hub.bindLog(); got[len(got)-1] != "local-3=/home/alice/src" {
		t.Errorf("binds %v", got)
	}
	a.answer(req, map[string]any{})
	if r := ed.recv(); r["id"] != float64(3) {
		t.Errorf("editor got %v", r)
	}
}

func TestInlining(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	small := filepath.Join(dir, "small.go")
	big := filepath.Join(dir, "big.txt")
	binary := filepath.Join(dir, "bin")
	fifo := filepath.Join(dir, "fifo")
	exact := filepath.Join(dir, "exact.txt")
	must(t, os.WriteFile(small, []byte("package main\n// héllo\n"), 0o600))
	must(t, os.WriteFile(big, []byte(strings.Repeat("a", acpbridge.MaxInline+1)), 0o600))
	must(t, os.WriteFile(exact, []byte(strings.Repeat("b", acpbridge.MaxInline)), 0o600))
	must(t, os.WriteFile(binary, []byte{0xff, 0xfe, 0x00}, 0o600))
	must(t, syscall.Mkfifo(fifo, 0o600))

	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	links := []string{"file://" + small, "file://" + exact, "file://" + big, "file://" + binary, "file://" + fifo,
		"file://" + dir, "file://" + filepath.Join(dir, "missing"), "https://example.com/x", "file://other-host" + small}
	blocks := make([]string, 0, len(links))
	for _, l := range links {
		b, err := json.Marshal(map[string]string{"type": "resource_link", "uri": l, "name": "x"})
		must(t, err)
		blocks = append(blocks, string(b))
	}
	ed.send(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[` + strings.Join(blocks, ",") + `,{"type":"text","text":"hi"}]}}`)
	req := a.recv()
	got, ok := param(req, "prompt").([]any)
	if !ok {
		t.Fatalf("prompt %v is not a list", param(req, "prompt"))
	}
	for i, l := range links {
		blk := asMap(t, got[i])
		inlined := i < 2
		if !inlined {
			if blk["type"] != "resource_link" || blk["uri"] != l {
				t.Errorf("%s: changed to %v", l, blk)
			}
			continue
		}
		res, _ := blk["resource"].(map[string]any)
		want, _ := os.ReadFile(strings.TrimPrefix(l, "file://"))
		if blk["type"] != "resource" || res["uri"] != l || res["mimeType"] != "text/plain" || res["text"] != string(want) {
			t.Errorf("%s: not inlined: type %v uri %v", l, blk["type"], res["uri"])
		}
	}
	if blk := asMap(t, got[len(links)]); blk["text"] != "hi" {
		t.Errorf("text block %v", blk)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestReconnectReplay drops the ACP leg the ways acp-host and the hub drop
// it, and checks that the bridge answers the request in flight with
// MsgLost, reconnects, replays initialize and session/load without passing
// the replayed history to the editor, and then serves the editor again.
func TestReconnectReplay(t *testing.T) {
	t.Parallel()
	closeWith := func(code websocket.StatusCode, reason string) func(*agent) {
		return func(a *agent) { _ = a.conn.Close(code, reason) }
	}
	dropTCP := func(a *agent) { _ = a.conn.CloseNow() }
	for _, tc := range []struct {
		name string
		drop func(*agent)
		// outage makes the hub answer upgrades with 502 after the drop,
		// as the ingress does while the hub restarts.
		outage bool
	}{
		{"acp-host child exited", closeWith(websocket.StatusInternalError, "hermes acp exited"), false},
		{"acp-host connection lost", dropTCP, false},
		{"gateway restart", closeWith(websocket.StatusGoingAway, "gateway restart"), false},
		{"hub shutting down then 502", closeWith(websocket.StatusGoingAway, "hub shutting down"), true},
		{"hub connection lost then 502", dropTCP, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testReconnectReplay(t, tc.drop, tc.outage)
		})
	}
}

// openSessions initializes, opens local-1 (new) and local-2 (load), passes
// a permission request to the editor and leaves prompt 4 in flight. It
// returns the editor-side id of the permission request.
func openSessions(t *testing.T, ed *editor, a *agent) string {
	t.Helper()
	ed.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"zed"}}}`)
	a.answer(a.recv(), map[string]any{keyProtocolVersion: 1})
	ed.recv()
	ed.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/alice/src/app","mcpServers":[]}}`)
	a.answer(a.recv(), map[string]any{keySessionID: localSession})
	ed.recv()
	ed.send(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"local-2","cwd":"/home/alice/src/lib","mcpServers":[]}}`)
	req := a.recv()
	a.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-2","update":{"sessionUpdate":"user_message_chunk"}}}`)
	if r := ed.recv(); r["method"] != methodUpdate {
		t.Fatalf("history of an editor load not passed: %v", r)
	}
	a.answer(req, map[string]any{})
	ed.recv()

	a.send(`{"jsonrpc":"2.0","id":5,"method":"session/request_permission","params":{"sessionId":"local-1"}}`)
	stale, ok := ed.recv()["id"].(string)
	if !ok {
		t.Fatal("permission request reached the editor without a string id")
	}
	ed.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"local-1","prompt":[]}}`)
	a.recv()
	return stale
}

func testReconnectReplay(t *testing.T, drop func(*agent), outage bool) {
	t.Helper()
	hub := newFakeHub(t)
	ed := startBridgeReady(t, hub, 2*time.Second)
	a := hub.accept()
	stale := openSessions(t, ed, a)

	if outage {
		hub.setRefuse(http.StatusBadGateway)
	}
	drop(a)
	r := ed.recv()
	if code, msg := errCode(r); r["id"] != float64(4) || code != acpbridge.CodeInternal || msg != acpbridge.MsgLost {
		t.Fatalf("in-flight prompt got %v", r)
	}
	if outage {
		ed.send(`{"jsonrpc":"2.0","id":5,"method":"session/list","params":{}}`)
		if code, msg := errCode(ed.recv()); code != acpbridge.CodeInternal || msg != acpbridge.MsgLost {
			t.Fatalf("request during the outage got %d %q", code, msg)
		}
		if n := hub.setRefuse(0); n < 2 {
			t.Fatalf("bridge stopped retrying after %d refused upgrades", n)
		}
	}

	b := hub.accept()
	init := b.recv()
	initID, _ := init["id"].(string)
	if init["method"] != methodInitialize || !strings.HasPrefix(initID, acpbridge.IDPrefix) {
		t.Fatalf("first message on reconnect %v", init)
	}
	if info, _ := param(init, "clientInfo").(map[string]any); info["name"] != "zed" {
		t.Errorf("initialize params not the editor's: %v", init["params"])
	}
	ed.send(`{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{"sessionId":"local-1","prompt":[]}}`)
	b.answer(init, map[string]any{keyProtocolVersion: 1})
	for _, want := range []struct{ sid, cwd string }{{localSession, "/home/alice/src/app"}, {"local-2", "/home/alice/src/lib"}} {
		load := b.recv()
		if load["method"] != "session/load" || param(load, keySessionID) != want.sid || param(load, "cwd") != want.cwd {
			t.Fatalf("replay load %v", load)
		}
		if s, ok := param(load, "mcpServers").([]any); !ok || len(s) != 0 {
			t.Errorf("mcpServers %v", param(load, "mcpServers"))
		}
		if id, _ := load["id"].(string); !strings.HasPrefix(id, acpbridge.IDPrefix) || id == initID {
			t.Errorf("load id %v", load["id"])
		}
		for _, kind := range []string{"user_message_chunk", "agent_message_chunk"} {
			b.send(map[string]any{keyJSONRPC: jsonRPC, "method": methodUpdate, "params": map[string]any{keySessionID: want.sid, "update": map[string]any{"sessionUpdate": kind}}})
		}
		b.answer(load, map[string]any{})
	}
	waiting := b.recv()
	if waiting["method"] != "session/prompt" {
		t.Fatalf("editor request sent during the replay: agent got %v", waiting)
	}
	ed.none()
	b.answer(waiting, map[string]any{"stopReason": "end_turn"})
	if r := ed.recv(); r["id"] != float64(6) || r["result"] == nil {
		t.Fatalf("editor got %v", r)
	}

	ed.send(`{"jsonrpc":"2.0","id":"` + stale + `","result":{"outcome":{"outcome":"cancelled"}}}`)
	ed.send(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"local-1","prompt":[]}}`)
	if got := b.recv(); got["method"] != "session/prompt" {
		t.Fatalf("stale answer reached the new agent, or the prompt did not: %v", got)
	}
	b.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-1","update":{"sessionUpdate":"agent_message_chunk"}}}`)
	if r := ed.recv(); r["method"] != methodUpdate {
		t.Errorf("live update after replay %v", r)
	}
	if len(hub.bindLog()) != 2 {
		t.Errorf("replay bound again: %v", hub.bindLog())
	}
	if !outage && hub.refusedCount() != 0 {
		t.Errorf("hub refused %d upgrades", hub.refusedCount())
	}
}

func TestReplayDropsFailedLoad(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	ed.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	a.answer(a.recv(), map[string]any{})
	ed.recv()
	ed.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/alice","mcpServers":[]}}`)
	a.answer(a.recv(), map[string]any{keySessionID: localSession})
	ed.recv()

	_ = a.conn.Close(websocket.StatusGoingAway, "")
	b := hub.accept()
	b.answer(b.recv(), map[string]any{})
	load := b.recv()
	b.send(map[string]any{keyJSONRPC: jsonRPC, "id": load["id"], "error": map[string]any{"code": -32002, "message": "not found"}})
	ed.none()

	_ = b.conn.Close(websocket.StatusGoingAway, "")
	c := hub.accept()
	if init := c.recv(); init["method"] != methodInitialize {
		t.Fatalf("got %v", init)
	} else {
		c.answer(init, map[string]any{})
	}
	ed.send(`{"jsonrpc":"2.0","id":3,"method":"session/list","params":{}}`)
	if got := c.recv(); got["method"] != "session/list" {
		t.Errorf("a session that failed to load was loaded again: %v", got)
	}
}

func TestUnauthorized(t *testing.T) {
	t.Parallel()
	t.Run("upgrade refused", func(t *testing.T) {
		t.Parallel()
		hub := newFakeHub(t)
		hub.refuse = true
		ed := startBridge(t, hub)
		select {
		case err := <-ed.done:
			if !errors.Is(err, hubapi.ErrUnauthorized) {
				t.Errorf("Run = %v", err)
			}
			ed.done <- err
		case <-time.After(10 * time.Second):
			t.Fatal("bridge did not stop")
		}
	})
	t.Run("closed with 4401", func(t *testing.T) {
		t.Parallel()
		hub := newFakeHub(t)
		ed := startBridge(t, hub)
		a := hub.accept()
		_ = a.conn.Close(protocol.CloseUnauthorized, "revoked")
		select {
		case err := <-ed.done:
			if !errors.Is(err, hubapi.ErrUnauthorized) {
				t.Errorf("Run = %v", err)
			}
			ed.done <- err
		case <-time.After(10 * time.Second):
			t.Fatal("bridge did not stop")
		}
	})
}

func TestEditorEOF(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	hub.accept()
	_ = ed.in.Close()
	select {
	case err := <-ed.done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
		ed.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not stop on EOF")
	}
}

func TestEditorIDs(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	for _, id := range []string{`"hermes-device-1"`, `"\u00e9"`, `1e0`, `null`, `-3`} {
		ed.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"session/set_mode","params":{"sessionId":"s","modeId":"m"}}`)
		req := a.recv()
		ownID, _ := req["id"].(string)
		if !strings.HasPrefix(ownID, acpbridge.IDPrefix) {
			t.Fatalf("agent got id %v", req["id"])
		}
		a.send(`{"jsonrpc":"2.0","id":"` + ownID + `","result":{}}`)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(ed.recvLine().raw, &m)
		if string(m["id"]) != id {
			t.Errorf("id %s came back as %s", id, m["id"])
		}
	}
	ed.send(`[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`)
	if code, _ := errCode(ed.recv()); code != acpbridge.CodeInvalidReq {
		t.Errorf("batch code %d", code)
	}
}

func TestReadyTimeout(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	hub.refuse = true
	hub.status = http.StatusBadGateway
	ed := startBridge(t, hub)
	ed.send(`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`)
	if code, msg := errCode(ed.recv()); code != acpbridge.CodeInternal || msg != acpbridge.MsgLost {
		t.Errorf("got %d %q", code, msg)
	}
}

func TestSessionClose(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	ed.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	a.answer(a.recv(), map[string]any{})
	ed.recv()
	ed.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/alice","mcpServers":[]}}`)
	a.answer(a.recv(), map[string]any{keySessionID: localSession})
	ed.recv()
	ed.send(`{"jsonrpc":"2.0","id":3,"method":"session/close","params":{"sessionId":"local-1","_meta":{}}}`)
	r := ed.recvLine()
	if string(r.raw) != `{"id":3,"jsonrpc":"2.0","result":{}}` {
		t.Errorf("editor got %s", r.raw)
	}
	got := a.recv()
	if got["method"] != "session/cancel" || got["id"] != nil {
		t.Fatalf("agent got %v", got)
	}
	if p := asMap(t, got["params"]); len(p) != 1 || p[keySessionID] != localSession {
		t.Errorf("cancel params %v", p)
	}

	ed.send(`{"jsonrpc":"2.0","id":4,"method":"session/close","params":{"sessionId":"local-1"}}`)
	if r := ed.recv(); r["id"] != float64(4) || len(asMap(t, r["result"])) != 0 {
		t.Errorf("editor got %v", r)
	}
	ed.send(`{"jsonrpc":"2.0","id":5,"method":"session/close","params":{}}`)
	if code, _ := errCode(ed.recv()); code != acpbridge.CodeInvalidParams {
		t.Errorf("close without a session: code %d", code)
	}
	ed.send(`{"jsonrpc":"2.0","id":6,"method":"session/set_mode","params":{"sessionId":"s","modeId":"m"}}`)
	if got := a.recv(); got["method"] != "session/set_mode" {
		t.Errorf("a second close reached the agent: %v", got)
	}

	_ = a.conn.Close(websocket.StatusGoingAway, "")
	ed.recv()
	b := hub.accept()
	b.answer(b.recv(), map[string]any{})
	ed.send(`{"jsonrpc":"2.0","id":7,"method":"session/list","params":{}}`)
	if got := b.recv(); got["method"] != "session/list" {
		t.Errorf("a closed session was loaded again: %v", got)
	}
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

func TestStripsTopLevelMeta(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()

	ed.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"_meta":{"x":1}},"_meta":{"session_id":"other"}}}`)
	req := a.recv()
	p := asMap(t, req["params"])
	if _, ok := p["_meta"]; ok {
		t.Errorf("initialize kept _meta: %v", p)
	}
	if asMap(t, p["clientCapabilities"])["_meta"] == nil {
		t.Errorf("a nested _meta was removed: %v", p)
	}
	a.answer(req, map[string]any{keyProtocolVersion: 1})
	ed.recv()

	ed.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"hi","_meta":{"k":"v"}}],"_meta":{"cwd":"/"}}}`)
	req = a.recv()
	p = asMap(t, req["params"])
	if _, ok := p["_meta"]; ok || p[keySessionID] != "s" {
		t.Errorf("prompt params %v", p)
	}
	blocks, _ := p["prompt"].([]any)
	if len(blocks) != 1 || asMap(t, blocks[0])["_meta"] == nil {
		t.Errorf("prompt blocks %v", blocks)
	}
	a.answer(req, map[string]any{"stopReason": "end_turn"})
	ed.recv()

	ed.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s","_meta":null}}`)
	got := a.recv()
	if got["method"] != "session/cancel" || got["id"] != nil {
		t.Fatalf("agent got %v", got)
	}
	if p := asMap(t, got["params"]); len(p) != 1 || p[keySessionID] != "s" {
		t.Errorf("cancel params %v", p)
	}
}

func TestInitializeAdvertisesClose(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()

	ed.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	a.send(`{"jsonrpc":"2.0","id":"` + str(a.recv()["id"]) + `","result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"fork":{},"list":{},"resume":{}}},"authMethods":[]}}`)
	res := asMap(t, ed.recv()["result"])
	caps := asMap(t, res["agentCapabilities"])
	sc := asMap(t, caps["sessionCapabilities"])
	for _, k := range []string{"close", "fork", "list", "resume"} {
		if _, ok := sc[k].(map[string]any); !ok {
			t.Errorf("sessionCapabilities.%s missing: %v", k, sc)
		}
	}
	if caps["loadSession"] != true || res[keyProtocolVersion] != float64(1) || res["authMethods"] == nil {
		t.Errorf("result changed: %v", res)
	}

	ed.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1}}`)
	a.answer(a.recv(), map[string]any{keyProtocolVersion: 1})
	sc = asMap(t, asMap(t, asMap(t, ed.recv()["result"])["agentCapabilities"])["sessionCapabilities"])
	if len(sc) != 1 || sc["close"] == nil {
		t.Errorf("sessionCapabilities %v", sc)
	}

	ed.send(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":1}}`)
	a.answer(a.recv(), map[string]any{keyProtocolVersion: 1, "agentCapabilities": "odd"})
	if r := asMap(t, ed.recv()["result"]); r["agentCapabilities"] != "odd" {
		t.Errorf("a result that is not ACP changed: %v", r)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
