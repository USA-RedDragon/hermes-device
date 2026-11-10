package device_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/coder/websocket"
)

const (
	keySessionACP  = "sessionId"
	keyMCPServers  = "mcpServers"
	keyPrompt      = "prompt"
	keyParams      = "params"
	acpLoad        = "session/load"
	acpPrompt      = "session/prompt"
	sessionMine    = "s-mine"
	sessionOther   = "s-other"
	sessionUnbound = "s-unbound"
	otherDevice    = "d_other"
	acpInitialize  = "initialize"
	keyText        = "text"
	acpUpdate      = "session/update"
	acpCancel      = "session/cancel"
)

type acpConn struct {
	t  *testing.T
	c  *websocket.Conn
	e  *env
	id int
}

func (e *env) acpLeg(token string) *acpConn {
	e.t.Helper()
	c, _, err := dial(e.t, e.a, "/api/device/acp", token)
	if err != nil {
		e.t.Fatal(err)
	}
	return &acpConn{t: e.t, c: c, e: e}
}

// request sends one request under a bridge-style id and returns the answer.
func (a *acpConn) request(method string, params any) map[string]any {
	a.t.Helper()
	a.id++
	id := fmt.Sprintf("hermes-device-%d", a.id)
	msg := map[string]any{keyJSONRPC: jsonRPC, "id": id, keyMethod: method}
	if params != nil {
		msg[keyParams] = params
	}
	writeJSON(a.t, a.c, msg)
	m := readJSON(a.t, a.c)
	if m["id"] != id {
		a.t.Fatalf("%s answer = %v, want id %s", method, m, id)
	}
	return m
}

// lastAgentMessage sends an initialize as a fence and returns what the
// agent got just before it.
func (a *acpConn) lastAgentMessage() map[string]any {
	a.t.Helper()
	a.request(acpInitialize, map[string]any{})
	got := a.e.acp.Received()
	if len(got) < 2 {
		return nil
	}
	return got[len(got)-2]
}

func errCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestACPLegMethods(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		a := e.acpLeg(tok)
		if m := a.request(acpInitialize, map[string]any{"protocolVersion": 1}); m["result"] == nil {
			t.Fatalf("initialize = %v", m)
		}
		if m := a.request("authenticate", map[string]any{"methodId": "x"}); m["result"] == nil {
			t.Fatalf("authenticate = %v", m)
		}
		before := len(e.acp.Received())
		for _, method := range []string{"session/close", "_hermes/ext", "fs/read_text_file", "terminal/create", acpUpdate,
			acpCancel, "session/request_permission"} {
			if m := a.request(method, map[string]any{keySessionACP: "x"}); errCode(m) != -32601 || reason(m) != reasonDenied {
				t.Fatalf("%s = %v", method, m)
			}
		}
		writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, keyMethod: "session/new", keyParams: map[string]any{}})
		writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, keyMethod: "_hermes/note", keyParams: map[string]any{}})
		if m := a.request("session/list", "x"); errCode(m) != -32602 {
			t.Fatalf("list with string params = %v", m)
		}
		if got := e.acp.Received(); len(got) != before {
			t.Fatalf("refused messages reached the agent: %v", got[before:])
		}
		writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, "id": "hermes-hub-a1", keyMethod: acpInitialize})
		if m := readJSON(t, a.c); errCode(m) != -32600 {
			t.Fatalf("hub id = %v", m)
		}
		if err := a.c.Write(context.Background(), websocket.MessageText, []byte("{")); err != nil {
			t.Fatal(err)
		}
		if m := readJSON(t, a.c); errCode(m) != -32700 {
			t.Fatalf("parse error = %v", m)
		}
		writeJSON(t, a.c, []any{map[string]any{keyJSONRPC: jsonRPC, "id": 1, keyMethod: acpInitialize}})
		if m := readJSON(t, a.c); errCode(m) != -32600 {
			t.Fatalf("batch = %v", m)
		}
	})
}

func TestACPLegSessions(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		e.plugin.SetBinding(sessionOther, otherDevice, "/home/bob")
		e.plugin.SetParent("s-child", sessionMine)
		a := e.acpLeg(tok)
		m := a.request("session/new", map[string]any{keyCWD: deviceDir, keyMCPServers: []any{map[string]any{keyName: "x", "command": "sh"}},
			"mcp_servers": []any{"y"}})
		res, _ := m["result"].(map[string]any)
		created, _ := res[keySessionACP].(string)
		if created == "" {
			t.Fatalf("new = %v", m)
		}
		sent := a.lastAgentMessage()
		p, _ := sent[keyParams].(map[string]any)
		if mcp, ok := p[keyMCPServers].([]any); !ok || len(mcp) != 0 || p["mcp_servers"] != nil {
			t.Fatalf("new reached the agent as %v", sent)
		}
		if m := a.request(acpPrompt, map[string]any{keySessionACP: created, keyPrompt: []any{
			map[string]any{keyType: "resource_link", "uri": "file:///etc/passwd", keyName: "p"}, map[string]any{keyType: keyText, keyText: "hi"},
		}}); m["result"] == nil {
			t.Fatalf("prompt in a created session = %v", m)
		}
		sent = a.lastAgentMessage()
		p, _ = sent[keyParams].(map[string]any)
		blocks, _ := p[keyPrompt].([]any)
		var first map[string]any
		if len(blocks) == 2 {
			first, _ = blocks[0].(map[string]any)
		}
		if text, _ := first[keyText].(string); first[keyType] != keyText || !strings.Contains(text, "file:///etc/passwd removed") {
			t.Fatalf("prompt reached the agent as %v", sent)
		}
		for _, sid := range []string{sessionMine, "s-child"} {
			for _, method := range []string{acpLoad, "session/resume", "session/fork", acpPrompt, "session/set_mode", "session/set_model",
				"session/set_config_option"} {
				if m := a.request(method, map[string]any{keySessionACP: sid, keyCWD: deviceDir}); m["result"] == nil {
					t.Fatalf("%s of %s = %v", method, sid, m)
				}
			}
		}
		for _, params := range []map[string]any{
			{keySessionACP: sessionOther}, {"session_id": sessionOther}, {keySessionACP: sessionMine, "session_id": sessionOther},
		} {
			if m := a.request(acpLoad, params); errCode(m) != -32602 || reason(m) != "hermes_device_other_device" {
				t.Fatalf("load %v = %v", params, m)
			}
		}
		for _, method := range []string{acpLoad, acpPrompt, "session/set_mode"} {
			if m := a.request(method, map[string]any{keySessionACP: sessionUnbound}); errCode(m) != -32602 || reason(m) != "hermes_device_not_bound" {
				t.Fatalf("%s of an unbound session = %v", method, m)
			}
		}
		for _, params := range []map[string]any{{}, {keySessionACP: 7}, {keySessionACP: ""}} {
			if m := a.request(acpPrompt, params); errCode(m) != -32602 {
				t.Fatalf("prompt %v = %v", params, m)
			}
		}
		n := len(e.acp.Received())
		writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, keyMethod: acpCancel, keyParams: map[string]any{keySessionACP: sessionOther}})
		writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, keyMethod: acpCancel, keyParams: map[string]any{keySessionACP: sessionMine}})
		if sent := a.lastAgentMessage(); len(e.acp.Received()) != n+2 || sent[keyMethod] != acpCancel {
			t.Fatalf("cancels reached the agent as %v", e.acp.Received()[n:])
		}
		m = a.request("session/new", map[string]any{keyCWD: deviceDir})
		res, _ = m["result"].(map[string]any)
		taken, _ := res[keySessionACP].(string)
		e.plugin.SetBinding(taken, otherDevice, "/home/bob")
		if m := a.request(acpPrompt, map[string]any{keySessionACP: taken}); reason(m) != "hermes_device_other_device" {
			t.Fatalf("prompt in a created session another device bound = %v", m)
		}
	})
}

func TestACPLegList(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		e.plugin.SetBinding(sessionOther, otherDevice, "/home/bob")
		e.acp.SetSessions([]map[string]any{
			{keySessionACP: sessionMine, keyCWD: deviceDir}, {keySessionACP: sessionOther, keyCWD: "/home/bob"}, {keySessionACP: sessionUnbound},
		})
		a := e.acpLeg(tok)
		m := a.request("session/list", map[string]any{})
		res, _ := m["result"].(map[string]any)
		rows, _ := res["sessions"].([]any)
		if len(rows) != 1 {
			t.Fatalf("list = %v", m)
		}
		if row, _ := rows[0].(map[string]any); row[keySessionACP] != sessionMine {
			t.Fatalf("list = %v", m)
		}
	})
}

func TestACPLegAgentRequests(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		a := e.acpLeg(tok)
		a.request(acpInitialize, map[string]any{})
		ctx := context.Background()
		send := func(msg map[string]any) {
			t.Helper()
			msg[keyJSONRPC] = jsonRPC
			if err := e.acp.Send(ctx, msg); err != nil {
				t.Fatal(err)
			}
		}
		n := len(e.acp.Received())
		send(map[string]any{"id": 5, keyMethod: "fs/read_text_file", keyParams: map[string]any{keySessionACP: "x", "path": "/home/alice/.ssh/id_ed25519"}})
		send(map[string]any{"id": 6, keyMethod: "terminal/create", keyParams: map[string]any{keySessionACP: "x", "command": "sh"}})
		send(map[string]any{keyMethod: "_hermes/note", keyParams: map[string]any{}})
		send(map[string]any{"id": 9, "result": map[string]any{}})
		send(map[string]any{keyMethod: acpUpdate, keyParams: map[string]any{keySessionACP: "x", "update": map[string]any{}}})
		if m := readJSON(t, a.c); m[keyMethod] != acpUpdate {
			t.Fatalf("device got %v before the update", m)
		}
		got := e.acp.WaitReceived(ctx, n+2)[n:]
		if len(got) != 2 || got[0]["id"] != float64(5) || errCode(got[0]) != -32601 || got[1]["id"] != float64(6) || errCode(got[1]) != -32601 {
			t.Fatalf("agent got %v", got)
		}
		for i, method := range []string{"session/request_permission", "elicitation/create"} {
			send(map[string]any{"id": 20 + i, keyMethod: method, keyParams: map[string]any{keySessionACP: "x"}})
			m := readJSON(t, a.c)
			if m[keyMethod] != method || m["id"] != float64(20+i) {
				t.Fatalf("device got %v", m)
			}
			writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, "id": 20 + i, "result": map[string]any{"outcome": "x"}})
			if r := e.acp.WaitReceived(ctx, n+3+i)[n+2+i]; r["id"] != float64(20+i) || r["result"] == nil {
				t.Fatalf("agent got %v", r)
			}
		}
	})
}
