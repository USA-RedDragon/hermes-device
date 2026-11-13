//go:build unix

package acpbridge_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/acpbridge"
	"github.com/coder/websocket"
)

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
	if r["id"] != float64(1) || code != acpbridge.CodeInternal || !strings.HasPrefix(msg, "talaria: could not bind the session to /home/alice/offline: ") ||
		!strings.Contains(msg, "device_offline") {
		t.Errorf("editor got %v", r)
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
	b.send(map[string]any{keyJSONRPC: jsonRPC, "id": load["id"], keyError: map[string]any{keyCode: -32002, keyMessage: "not found"}})
	ed.none()

	_ = b.conn.Close(websocket.StatusGoingAway, "")
	c := hub.accept()
	if init := c.recv(); init["method"] != methodInitialize {
		t.Fatalf("got %v", init)
	} else {
		c.answer(init, map[string]any{})
	}
	ed.send(`{"jsonrpc":"2.0","id":3,"method":"session/list","params":{}}`)
	if got := c.recv(); got["method"] != methodList {
		t.Errorf("a session that failed to load was loaded again: %v", got)
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
	if got := b.recv(); got["method"] != methodList {
		t.Errorf("a closed session was loaded again: %v", got)
	}
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
	a.answer(a.recv(), map[string]any{keySessionID: session2})
	if r := ed.recv(); r["id"] != "f1" || r[keyError] != nil {
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
