package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/protocol"
)

// MoveCommand is the prompt that moves a chat the editor opened from
// another place to the editor's folder (docs/protocol.md 6.3).
const MoveCommand = "/move-here"

// JSON-RPC and ACP members the bridge builds messages with.
const (
	keySessionID = "sessionId"
	keyMethod    = "method"
	keyParams    = "params"
	keyResult    = "result"
	keyText      = "text"
)

// place says where an open session runs, as the bridge knows it.
type place int

const (
	// placeHere is a session bound to this device and the editor's folder.
	placeHere place = iota
	// placeOtherDir is a session bound to this device, loaded, whose
	// binding is another folder or a folder acp-host did not report.
	placeOtherDir
	// placeNotHere is a session not bound to this device. The bridge
	// answered its load itself and Hermes has not loaded it.
	placeNotHere
)

// Reasons and messages the hub and acp-host refuse a session with
// (docs/protocol.md 6.1).
const (
	reasonOtherDevice = "hermes_device_other_device"
	reasonNotBound    = "hermes_device_not_bound"
	msgOtherDevice    = "session is bound to another device"
	msgNotBound       = "session is not bound to this device"
)

type refusal int

const (
	refusedOther refusal = iota + 1
	refusedNotBound
)

// bindingRefusal reports why the hub or acp-host refused a session in the
// error response m, or 0 when m is not such a refusal.
func bindingRefusal(m object) refusal {
	var e struct {
		Message string `json:"message"`
		Data    struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if json.Unmarshal(m["error"], &e) != nil {
		return 0
	}
	switch {
	case e.Data.Reason == reasonOtherDevice || e.Message == msgOtherDevice:
		return refusedOther
	case e.Data.Reason == reasonNotBound || e.Message == msgNotBound:
		return refusedNotBound
	}
	return 0
}

// takeBinding removes the binding acp-host added to a load or resume result
// and returns its dir, or "" when there is none.
func takeBinding(result json.RawMessage) (json.RawMessage, string) {
	res, ok := member(result)
	if !ok {
		return result, ""
	}
	meta, ok := member(res[keyMeta])
	if !ok {
		return result, ""
	}
	raw, has := meta[protocol.ACPBindingMeta]
	if !has {
		return result, ""
	}
	var b struct {
		Dir string `json:"dir"`
	}
	_ = json.Unmarshal(raw, &b)
	delete(meta, protocol.ACPBindingMeta)
	if len(meta) == 0 {
		delete(res, keyMeta)
	} else {
		res[keyMeta] = mustJSON(meta)
	}
	return mustJSON(res), b.Dir
}

// sameDir reports whether the binding dir bound is the editor's cwd. The
// plugin stores the real path, so cwd counts also after its symlinks are
// resolved; so does bound, for a path stored with a symlink in it (on
// macOS, /var and /tmp are links into /private).
func sameDir(bound, cwd string) bool {
	if bound == "" {
		return false
	}
	if bound == cwd {
		return true
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	if resolved == bound {
		return true
	}
	target, err := filepath.EvalSymlinks(bound)
	return err == nil && target == resolved
}

// sessionPlace returns the place of an open session.
func (b *Bridge) sessionPlace(id string) (openSession, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sessions {
		if s.id == id {
			return s, true
		}
	}
	return openSession{}, false
}

func (b *Bridge) setPlace(id string, p place) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.sessions {
		if b.sessions[i].id == id {
			b.sessions[i].place = p
		}
	}
}

// notice sends the editor an agent message in a session.
func (b *Bridge) notice(session, text string) {
	b.writeEditor(mustJSON(object{
		keyJSONRPC: json.RawMessage(`"2.0"`), keyMethod: mustJSON(methodUpdate),
		keyParams: mustJSON(map[string]any{
			keySessionID: session,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]string{"type": keyText, keyText: text},
			},
		}),
	}))
}

func moveCommand() map[string]string {
	return map[string]string{
		"name":        strings.TrimPrefix(MoveCommand, "/"),
		"description": "Move this chat to this folder on this computer",
	}
}

// offerMove advertises MoveCommand in a session.
func (b *Bridge) offerMove(session string) {
	b.writeEditor(mustJSON(object{
		keyJSONRPC: json.RawMessage(`"2.0"`), keyMethod: mustJSON(methodUpdate),
		keyParams: mustJSON(map[string]any{
			keySessionID: session,
			"update": map[string]any{
				"sessionUpdate":     "available_commands_update",
				"availableCommands": []any{moveCommand()},
			},
		}),
	}))
}

// addMoveCommand adds MoveCommand to an available_commands_update of a
// session that is not in the editor's folder. Other messages are returned
// as they are.
func (b *Bridge) addMoveCommand(m object) object {
	p := params(m)
	s, ok := b.sessionPlace(str(p[keySessionID]))
	if !ok || s.place == placeHere {
		return m
	}
	u, ok := member(p["update"])
	if !ok || str(u["sessionUpdate"]) != "available_commands_update" {
		return m
	}
	cmds := make([]json.RawMessage, 0, 1)
	if json.Unmarshal(u["availableCommands"], &cmds) != nil {
		return m
	}
	u["availableCommands"] = mustJSON(append(cmds, mustJSON(moveCommand())))
	p["update"] = mustJSON(u)
	m[keyParams] = mustJSON(p)
	return m
}

func noticeOtherDir(bound, cwd string) string {
	if bound == "" {
		return fmt.Sprintf("This chat is bound to a folder on this computer that the server did not report, "+
			"so it was not moved to %s and cannot run here yet. To move it to %s, send %s.", cwd, cwd, MoveCommand)
	}
	return fmt.Sprintf("This chat is bound to %s on this computer, not to %s. It was not moved, so it cannot run here. "+
		"To move it to %s, send %s. To keep it where it is, open %s instead.", bound, cwd, cwd, MoveCommand, bound)
}

func noticeNotHere(cwd string) string {
	return fmt.Sprintf("This chat is not bound to this computer. It runs where the server runs chats that are not bound "+
		"to a computer (its sandbox, when it has one), and it was not moved. Its history shows here after a move. "+
		"To move it to %s on this computer, send %s. To keep it where it is, use it from the web or chat app.", cwd, MoveCommand)
}

const msgOtherComputer = "This chat is bound to another computer, so it was not opened here. " +
	"Use it on that computer, or move it with /device in the chat."

// otherDeviceError rewrites a refusal of another device's session for the
// editor, which shows the message.
func otherDeviceError(m object) []byte {
	var e struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(m["error"], &e)
	if e.Code == 0 {
		e.Code = CodeInvalidParams
	}
	return errorMsg(m["id"], e.Code, msgOtherComputer)
}

// promptText is the text of a prompt's text blocks, trimmed.
func promptText(m object) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(params(m)["prompt"], &blocks)
	var parts []string
	for _, blk := range blocks {
		if blk.Type == keyText {
			parts = append(parts, blk.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func endTurn(id json.RawMessage) []byte {
	return mustJSON(object{keyJSONRPC: json.RawMessage(`"2.0"`), "id": id, keyResult: json.RawMessage(`{"stopReason":"end_turn"}`)})
}

// guardPrompt answers a session/prompt itself when its session is not in
// the editor's folder, or when it is MoveCommand. It reports whether it
// did; otherwise the prompt is forwarded. A prompt of a session not in the
// editor's folder is never forwarded: only MoveCommand moves the session.
func (b *Bridge) guardPrompt(ctx context.Context, m object, withID bool) (bool, error) {
	session := str(params(m)[keySessionID])
	s, open := b.sessionPlace(session)
	move := strings.EqualFold(promptText(m), MoveCommand)
	switch {
	case !open:
		return false, nil
	case s.place == placeHere && !move:
		return false, nil
	case s.place == placeHere:
		b.notice(session, fmt.Sprintf("This chat is already bound to %s on this computer.", s.cwd))
	case !move:
		b.notice(session, fmt.Sprintf("This chat was not moved to %s, so this message was not sent. "+
			"To move the chat here, send %s.", s.cwd, MoveCommand))
	case !withID:
		return true, nil
	default:
		return b.move(ctx, m, s)
	}
	if withID {
		b.writeEditor(endTurn(m["id"]))
	}
	return true, nil
}

// move binds a session to the editor's folder on the user's MoveCommand.
// A session Hermes has not loaded is loaded then, and its history reaches
// the editor; the prompt is answered when the load is.
func (b *Bridge) move(ctx context.Context, m object, s openSession) (bool, error) {
	if err := b.bind(ctx, s.id, s.cwd); err != nil {
		b.notice(s.id, fmt.Sprintf("Could not move this chat to %s: %v", s.cwd, err))
		b.writeEditor(endTurn(m["id"]))
		if errors.Is(err, hubapi.ErrUnauthorized) {
			return true, err
		}
		return true, nil
	}
	b.log.Info("Moved a session to the editor's folder on the user's request", "session", s.id, "dir", s.cwd)
	if s.place == placeOtherDir {
		b.setPlace(s.id, placeHere)
		b.notice(s.id, movedText(s.cwd))
		b.writeEditor(endTurn(m["id"]))
		return true, nil
	}
	// Bound here now, so a reconnect loads it, and a lost load leaves it
	// for MoveCommand again.
	b.setPlace(s.id, placeOtherDir)
	load := object{
		keyJSONRPC: json.RawMessage(`"2.0"`), "id": m["id"], keyMethod: mustJSON(methodLoad),
		keyParams: mustJSON(map[string]any{keySessionID: s.id, "cwd": s.cwd, "mcpServers": []any{}}),
	}
	return true, b.forward(ctx, load, pendingReq{id: m["id"], method: methodLoad, params: load[keyParams],
		cwd: s.cwd, session: s.id, move: true}, true)
}

func movedText(cwd string) string {
	return fmt.Sprintf("Moved this chat to %s on this computer. Send your message again.", cwd)
}

// finishMove answers the MoveCommand prompt of a session Hermes loaded
// after the move.
func (b *Bridge) finishMove(p pendingReq, ok bool, m object) {
	if ok {
		b.setPlace(p.session, placeHere)
		b.notice(p.session, movedText(p.cwd))
	} else {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(m["error"], &e)
		b.notice(p.session, fmt.Sprintf("Moved this chat to %s, but Hermes could not load it: %s. Send %s to try again.",
			p.cwd, e.Message, MoveCommand))
	}
	b.writeEditor(endTurn(p.id))
}

// checkReplayed handles the result of a session/load the bridge sent again
// after a reconnect: a session in the editor's folder whose binding moved
// to another folder meanwhile is no longer run from the editor.
func (b *Bridge) checkReplayed(s openSession, result json.RawMessage) {
	_, bound := takeBinding(result)
	if s.place != placeHere || bound == "" || sameDir(bound, s.cwd) {
		return
	}
	b.setPlace(s.id, placeOtherDir)
	b.log.Info("A session moved to another folder while the leg was down", "session", s.id, "bound", bound, "dir", s.cwd)
	b.notice(s.id, noticeOtherDir(bound, s.cwd))
}

// finishLoad passes the answer to an editor's session/load or
// session/resume. A session bound to this device and the editor's folder
// opens as before. A session bound to another folder of this device opens
// with a notice and stays where it is. A session not bound to this device
// opens empty with a notice, without Hermes, and a session of another
// device is refused.
func (b *Bridge) finishLoad(p pendingReq, ok bool, m object) {
	if ok {
		result, bound := takeBinding(m[keyResult])
		m[keyResult] = result
		if sameDir(bound, p.cwd) {
			b.addSession(p.session, p.cwd, placeHere)
		} else {
			b.addSession(p.session, p.cwd, placeOtherDir)
			b.log.Info("Opened a session of another folder without moving it", "session", p.session, "bound", bound, "dir", p.cwd)
			b.notice(p.session, noticeOtherDir(bound, p.cwd))
		}
		b.writeEditor(mustJSON(m))
		return
	}
	switch bindingRefusal(m) {
	case refusedOther:
		b.writeEditor(otherDeviceError(m))
		return
	case refusedNotBound:
		b.addSession(p.session, p.cwd, placeNotHere)
		b.log.Info("Opened a session that is not bound to this device without moving it", "session", p.session, "dir", p.cwd)
		b.offerMove(p.session)
		b.notice(p.session, noticeNotHere(p.cwd))
		b.writeEditor(mustJSON(object{keyJSONRPC: json.RawMessage(`"2.0"`), "id": m["id"], keyResult: json.RawMessage("{}")}))
		return
	}
	b.writeEditor(mustJSON(m))
}
