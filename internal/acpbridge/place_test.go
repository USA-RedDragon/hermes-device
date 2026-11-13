package acpbridge_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/acpbridge"
	"github.com/USA-RedDragon/talaria/protocol"
	"github.com/coder/websocket"
)

const (
	methodLoad    = "session/load"
	methodPrompt  = "session/prompt"
	methodList    = "session/list"
	keyDir        = "dir"
	keyCode       = "code"
	keyMessage    = "message"
	keyError      = "error"
	keyStopReason = "stopReason"
	stopEndTurn   = "end_turn"
	session2      = "local-2"
	editorDir     = "/home/alice/src/app"
	otherDir      = "/home/alice/other"
)

func bound(dir string) map[string]any {
	return map[string]any{"_meta": map[string]any{protocol.ACPBindingMeta: map[string]any{keyDir: dir}}}
}

func refused(reason string) map[string]any {
	return map[string]any{keyCode: acpbridge.CodeInvalidParams, keyMessage: "refused", "data": map[string]any{"reason": reason}}
}

func initBridge(t *testing.T, ed *editor, a *agent) {
	t.Helper()
	ed.send(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{"protocolVersion":1}}`)
	a.answer(a.recv(), map[string]any{keyProtocolVersion: 1})
	ed.recv()
}

// load sends the editor's session/load of sid in dir and returns the
// request the agent got.
func load(t *testing.T, ed *editor, a *agent, id, sid, dir string) map[string]any {
	t.Helper()
	ed.send(`{"jsonrpc":"2.0","id":"` + id + `","method":"session/load","params":{"sessionId":"` + sid + `","cwd":"` + dir + `","mcpServers":[]}}`)
	req := a.recv()
	if req["method"] != methodLoad || param(req, keySessionID) != sid || param(req, "cwd") != dir {
		t.Fatalf("agent got %v", req)
	}
	return req
}

func prompt(ed *editor, id, sid, text string) {
	ed.send(`{"jsonrpc":"2.0","id":"` + id + `","method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[{"type":"text","text":"` + text + `"}]}}`)
}

// noticeText returns the text of an agent_message_chunk update, or fails.
func noticeText(t *testing.T, m map[string]any) string {
	t.Helper()
	u, _ := param(m, "update").(map[string]any)
	c, _ := u["content"].(map[string]any)
	if m["method"] != methodUpdate || u["sessionUpdate"] != "agent_message_chunk" || c["type"] != "text" {
		t.Fatalf("editor got %v, want a notice", m)
	}
	text, _ := c["text"].(string)
	return text
}

func wantEndTurn(t *testing.T, m map[string]any, id string) {
	t.Helper()
	if m["id"] != id || asMap(t, m["result"])[keyStopReason] != stopEndTurn {
		t.Fatalf("editor got %v, want end_turn for %s", m, id)
	}
}

// wantRefusedPrompt checks that a prompt of a session that was not moved
// is answered by the bridge and never reaches the agent.
func wantRefusedPrompt(t *testing.T, ed *editor, a *agent, sid string) {
	t.Helper()
	prompt(ed, "p-refused", sid, "hi")
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "not sent") || !strings.Contains(text, acpbridge.MoveCommand) {
		t.Errorf("refusal notice %q", text)
	}
	wantEndTurn(t, ed.recv(), "p-refused")
	ed.send(`{"jsonrpc":"2.0","id":"list","method":"session/list","params":{}}`)
	req := a.recv()
	if req["method"] != methodList {
		t.Fatalf("agent got %v, want the list sent after the refused prompt", req)
	}
	a.answer(req, map[string]any{"sessions": []any{}})
	if r := ed.recv(); r["id"] != "list" {
		t.Fatalf("editor got %v", r)
	}
}

// wantForwarded checks that the agent gets a prompt of sid, as a sign that
// nothing else reached it before.
func wantForwarded(t *testing.T, ed *editor, a *agent, sid string) {
	t.Helper()
	prompt(ed, "p-fwd", sid, "probe")
	req := a.recv()
	if req["method"] != methodPrompt || param(req, keySessionID) != sid {
		t.Fatalf("agent got %v, want the prompt of %s", req, sid)
	}
	a.answer(req, map[string]any{keyStopReason: stopEndTurn})
	if r := ed.recv(); r["id"] != "p-fwd" {
		t.Fatalf("editor got %v", r)
	}
}

func TestLoadBoundHere(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	initBridge(t, ed, a)

	req := load(t, ed, a, "l1", localSession, editorDir)
	a.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-1","update":{"sessionUpdate":"user_message_chunk"}}}`)
	if r := ed.recv(); r["method"] != methodUpdate {
		t.Fatalf("history not passed: %v", r)
	}
	res := bound(editorDir)
	asMap(t, res["_meta"])["provenance"] = "p"
	a.answer(req, res)
	r := ed.recv()
	meta := asMap(t, asMap(t, r["result"])["_meta"])
	if r["id"] != "l1" || meta["provenance"] != "p" || meta[protocol.ACPBindingMeta] != nil {
		t.Fatalf("editor got %v", r)
	}
	wantForwarded(t, ed, a, localSession)
	prompt(ed, "m", localSession, acpbridge.MoveCommand)
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "already") {
		t.Errorf("move of a session bound here: %q", text)
	}
	wantEndTurn(t, ed.recv(), "m")
	if got := hub.bindLog(); len(got) != 0 {
		t.Errorf("a load bound: %v", got)
	}

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(target, link))
	a.answer(load(t, ed, a, "l2", session2, link), bound(target))
	if r := ed.recv(); r["id"] != "l2" || r["result"] == nil {
		t.Fatalf("load through a symlink got %v", r)
	}
	wantForwarded(t, ed, a, session2)
}

func TestLoadBoundToOtherFolder(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	initBridge(t, ed, a)

	req := load(t, ed, a, "l1", localSession, editorDir)
	a.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-1","update":{"sessionUpdate":"user_message_chunk"}}}`)
	ed.recv()
	a.answer(req, bound(otherDir))
	text := noticeText(t, ed.recv())
	if !strings.Contains(text, otherDir) || !strings.Contains(text, "not moved") || !strings.Contains(text, acpbridge.MoveCommand) {
		t.Errorf("notice %q", text)
	}
	if r := ed.recv(); r["id"] != "l1" || len(asMap(t, r["result"])) != 0 {
		t.Fatalf("editor got %v", r)
	}

	a.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-1","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"compress","description":"c"}]}}}`)
	u, _ := param(ed.recv(), "update").(map[string]any)
	cmds, _ := u["availableCommands"].([]any)
	if len(cmds) != 2 || asMap(t, cmds[1])["name"] != strings.TrimPrefix(acpbridge.MoveCommand, "/") {
		t.Errorf("commands %v", u["availableCommands"])
	}

	wantRefusedPrompt(t, ed, a, localSession)
	if got := hub.bindLog(); len(got) != 0 {
		t.Fatalf("bound without a choice: %v", got)
	}

	prompt(ed, "m", localSession, " "+acpbridge.MoveCommand+" ")
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "Moved this chat to "+editorDir) {
		t.Errorf("move notice %q", text)
	}
	wantEndTurn(t, ed.recv(), "m")
	if got := hub.bindLog(); len(got) != 1 || got[0] != localSession+"="+editorDir {
		t.Errorf("binds %v", got)
	}
	wantForwarded(t, ed, a, localSession)
}

func TestLoadWithoutReportedFolder(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	initBridge(t, ed, a)
	a.answer(load(t, ed, a, "l1", localSession, editorDir), map[string]any{})
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "did not report") {
		t.Errorf("notice %q", text)
	}
	ed.recv()
	wantRefusedPrompt(t, ed, a, localSession)
	if got := hub.bindLog(); len(got) != 0 {
		t.Errorf("binds %v", got)
	}
}

func TestLoadNotBoundHere(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	initBridge(t, ed, a)

	for _, e := range []map[string]any{
		refused("hermes_device_not_bound"),
		{keyCode: acpbridge.CodeInvalidParams, keyMessage: "session is not bound to this device"},
	} {
		req := load(t, ed, a, "l1", localSession, editorDir)
		a.send(map[string]any{keyJSONRPC: jsonRPC, "id": req["id"], keyError: e})
		u, _ := param(ed.recv(), "update").(map[string]any)
		if u["sessionUpdate"] != "available_commands_update" {
			t.Fatalf("no move command offered: %v", u)
		}
		if text := noticeText(t, ed.recv()); !strings.Contains(text, "not bound to this computer") || !strings.Contains(text, acpbridge.MoveCommand) {
			t.Errorf("notice %q", text)
		}
		if r := ed.recv(); r["id"] != "l1" || r[keyError] != nil || len(asMap(t, r["result"])) != 0 {
			t.Fatalf("editor got %v", r)
		}
	}
	wantRefusedPrompt(t, ed, a, localSession)
	if got := hub.bindLog(); len(got) != 0 {
		t.Fatalf("bound without a choice: %v", got)
	}

	prompt(ed, "m", localSession, acpbridge.MoveCommand)
	req := a.recv()
	if req["method"] != methodLoad || param(req, keySessionID) != localSession || param(req, "cwd") != editorDir {
		t.Fatalf("agent got %v after the move", req)
	}
	if got := hub.bindLog(); len(got) != 1 || got[0] != localSession+"="+editorDir {
		t.Errorf("binds %v", got)
	}
	a.send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"local-1","update":{"sessionUpdate":"user_message_chunk"}}}`)
	if r := ed.recv(); r["method"] != methodUpdate {
		t.Fatalf("history after the move not passed: %v", r)
	}
	a.answer(req, bound(editorDir))
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "Moved this chat to "+editorDir) {
		t.Errorf("move notice %q", text)
	}
	wantEndTurn(t, ed.recv(), "m")
	wantForwarded(t, ed, a, localSession)
}

func TestLoadOtherDevice(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridge(t, hub)
	a := hub.accept()
	initBridge(t, ed, a)
	for _, e := range []map[string]any{
		refused("hermes_device_other_device"),
		{keyCode: acpbridge.CodeInvalidParams, keyMessage: "session is bound to another device"},
	} {
		req := load(t, ed, a, "l1", localSession, editorDir)
		a.send(map[string]any{keyJSONRPC: jsonRPC, "id": req["id"], keyError: e})
		r := ed.recv()
		code, msg := errCode(r)
		if r["id"] != "l1" || code != acpbridge.CodeInvalidParams || !strings.Contains(msg, "another computer") {
			t.Fatalf("editor got %v", r)
		}
	}
	prompt(ed, "p", localSession, acpbridge.MoveCommand)
	if req := a.recv(); req["method"] != methodPrompt {
		t.Errorf("a session that did not open was handled: %v", req)
	}
	if got := hub.bindLog(); len(got) != 0 {
		t.Errorf("binds %v", got)
	}
}

func TestMoveBindFails(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	hub.failBind[editorDir] = http.StatusConflict
	ed := startBridge(t, hub)
	a := hub.accept()
	initBridge(t, ed, a)
	a.answer(load(t, ed, a, "l1", localSession, editorDir), bound(otherDir))
	ed.recv()
	ed.recv()
	prompt(ed, "m", localSession, acpbridge.MoveCommand)
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "Could not move") || !strings.Contains(text, "device_offline") {
		t.Errorf("notice %q", text)
	}
	wantEndTurn(t, ed.recv(), "m")
	wantRefusedPrompt(t, ed, a, localSession)
}

// TestReconnectKeepsPlaces checks that a reconnect loads again, without a
// bind or a notice, a session bound here and one the user moved, and
// leaves a session that was not moved for the user's choice.
func TestReconnectKeepsPlaces(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridgeReady(t, hub, 2*time.Second)
	a := hub.accept()
	initBridge(t, ed, a)

	a.answer(load(t, ed, a, "l1", localSession, editorDir), bound(editorDir))
	ed.recv()
	a.answer(load(t, ed, a, "l2", session2, editorDir), bound(otherDir))
	ed.recv()
	ed.recv()
	prompt(ed, "m", session2, acpbridge.MoveCommand)
	ed.recv()
	ed.recv()
	req := load(t, ed, a, "l3", "local-3", editorDir)
	a.send(map[string]any{keyJSONRPC: jsonRPC, "id": req["id"], keyError: refused("hermes_device_not_bound")})
	for range 3 {
		ed.recv()
	}
	binds := len(hub.bindLog())

	_ = a.conn.Close(websocket.StatusGoingAway, "")
	b := hub.accept()
	b.answer(b.recv(), map[string]any{})
	for _, sid := range []string{localSession, session2} {
		l := b.recv()
		if l["method"] != methodLoad || param(l, keySessionID) != sid {
			t.Fatalf("replay got %v, want a load of %s", l, sid)
		}
		b.answer(l, bound(editorDir))
	}
	ed.none()
	wantForwarded(t, ed, b, session2)
	wantForwarded(t, ed, b, localSession)
	if got := hub.bindLog(); len(got) != binds {
		t.Errorf("replay bound: %v", got)
	}
	wantRefusedPrompt(t, ed, b, "local-3")

	_ = b.conn.Close(websocket.StatusGoingAway, "")
	c := hub.accept()
	c.answer(c.recv(), map[string]any{})
	c.answer(c.recv(), bound(otherDir))
	if text := noticeText(t, ed.recv()); !strings.Contains(text, otherDir) {
		t.Errorf("notice after a move elsewhere during the outage: %q", text)
	}
	c.answer(c.recv(), bound(editorDir))
	wantRefusedPrompt(t, ed, c, localSession)
	wantForwarded(t, ed, c, session2)
}

// TestMoveLoadLost checks that a chat bound by MoveCommand whose load was
// lost with the leg is loaded on the reconnect and moved by MoveCommand
// again.
func TestMoveLoadLost(t *testing.T) {
	t.Parallel()
	hub := newFakeHub(t)
	ed := startBridgeReady(t, hub, 2*time.Second)
	a := hub.accept()
	initBridge(t, ed, a)
	req := load(t, ed, a, "l1", localSession, editorDir)
	a.send(map[string]any{keyJSONRPC: jsonRPC, "id": req["id"], keyError: refused("hermes_device_not_bound")})
	for range 3 {
		ed.recv()
	}
	ed.send(`{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"local-1","prompt":[{"type":"text","text":"/move-here"}]}}`)
	prompt(ed, "m", localSession, acpbridge.MoveCommand)
	if l := a.recv(); l["method"] != methodLoad {
		t.Fatalf("agent got %v", l)
	}
	if got := hub.bindLog(); len(got) != 1 {
		t.Fatalf("binds %v, want one: a move without an id does nothing", got)
	}
	_ = a.conn.Close(websocket.StatusGoingAway, "")
	if code, msg := errCode(ed.recv()); code != acpbridge.CodeInternal || msg != acpbridge.MsgLost {
		t.Fatalf("lost move got %d %q", code, msg)
	}
	b := hub.accept()
	b.answer(b.recv(), map[string]any{})
	l := b.recv()
	if l["method"] != methodLoad || param(l, keySessionID) != localSession {
		t.Fatalf("replay got %v", l)
	}
	b.answer(l, bound(editorDir))
	wantRefusedPrompt(t, ed, b, localSession)
	prompt(ed, "m2", localSession, acpbridge.MoveCommand)
	if text := noticeText(t, ed.recv()); !strings.Contains(text, "Moved") {
		t.Errorf("notice %q", text)
	}
	wantEndTurn(t, ed.recv(), "m2")
	wantForwarded(t, ed, b, localSession)
}
