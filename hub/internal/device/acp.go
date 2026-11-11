package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/coder/websocket"
)

// ACP methods the hub treats specially.
const (
	acpNew        = "session/new"
	acpLoad       = "session/load"
	acpResume     = "session/resume"
	acpFork       = "session/fork"
	acpList       = "session/list"
	acpPrompt     = "session/prompt"
	acpUpdate     = "session/update"
	keyACPSession = "sessionId"
	keyACPAlias   = "session_id"
	acpOwnPrefix  = OwnIDPrefix + "a"
)

// acpRule is how the hub lets one ACP method through.
type acpRule struct {
	// notification is set for a method that is sent without an id; the
	// others are requests.
	notification bool
	// session is set for a method that names a session in sessionId (or
	// its alias session_id), which must belong to the device.
	session bool
	// params are the members its params may have besides sessionId and
	// session_id, by alias and by field name: the ACP SDK (v0.9.0,
	// acp/schema.py) accepts both, and merges a _meta member into the
	// handler's arguments (acp/router.py _make_func), where it would
	// replace sessionId or cwd after the hub checked them. Every other
	// member, _meta included, is refused.
	params []string
}

// ACP params members of more than one method.
const (
	acpCWD       = "cwd"
	acpMCP       = "mcpServers"
	acpMCPField  = "mcp_servers"
	acpMetaField = "field_meta"
)

// acpEditorRules are the editor-to-agent methods a device may send on the
// ACP leg: those of the ACP SDK that Hermes v0.23.1's gateway ACP agent
// implements (acp_adapter/gateway_server.py). Others, such as
// session/close, which that agent does not implement, are refused.
//
//nolint:gochecknoglobals // a fixed table
var acpEditorRules = map[string]acpRule{
	"initialize": {params: []string{"protocolVersion", "protocol_version", "clientCapabilities", "client_capabilities",
		"clientInfo", "client_info"}},
	"authenticate":              {params: []string{"methodId", "method_id"}},
	acpNew:                      {params: []string{acpCWD, acpMCP, acpMCPField}},
	acpList:                     {params: []string{"cursor", acpCWD}},
	acpLoad:                     {session: true, params: []string{acpCWD, acpMCP, acpMCPField}},
	acpResume:                   {session: true, params: []string{acpCWD, acpMCP, acpMCPField}},
	acpFork:                     {session: true, params: []string{acpCWD, acpMCP, acpMCPField}},
	acpPrompt:                   {session: true, params: []string{"prompt", "messageId", "message_id"}},
	"session/set_mode":          {session: true, params: []string{"modeId", "mode_id"}},
	"session/set_model":         {session: true, params: []string{"modelId", "model_id"}},
	"session/set_config_option": {session: true, params: []string{"configId", "config_id", "type", "value"}},
	"session/cancel":            {notification: true, session: true},
}

// acpAgentRules are the agent-to-editor methods the hub relays to the
// device: those Hermes sends. The hub answers any other agent request,
// such as fs/read_text_file or terminal/create, which would act on the
// device outside the device's own rules, with an error.
//
//nolint:gochecknoglobals // a fixed table
var acpAgentRules = map[string]acpRule{
	acpUpdate:                    {notification: true},
	"session/request_permission": {},
	"elicitation/create":         {},
}

// ACPEditorMethods are the methods a device may send on the ACP leg.
func ACPEditorMethods() []string { return slices.Sorted(maps.Keys(acpEditorRules)) }

// ACPAgentMethods are the methods the hub relays from acp-host to a device.
func ACPAgentMethods() []string { return slices.Sorted(maps.Keys(acpAgentRules)) }

type acpPending struct {
	id     json.RawMessage
	method string
}

// acpGuard enforces the hub's ACP leg rules (docs/protocol.md 6): it parses
// every message, refuses methods outside the tables above and sessions of
// other devices, and repeats acp-host's MCP and resource_link rewrites, so
// acp-host's checks are not the only ones.
type acpGuard struct {
	s        *Server
	b        *backend
	dev      hub.Device
	l        *leg
	sessions *sessionCheck

	mu      sync.Mutex
	pending map[string]acpPending
	nextOwn uint64
	// created are the sessions acp-host created on this leg for the
	// device, by session/new or session/fork. They may be used before the
	// device binds them.
	created map[string]bool
	work    sync.WaitGroup
}

func newACPGuard(s *Server, b *backend, d hub.Device, l *leg) *acpGuard {
	return &acpGuard{s: s, b: b, dev: d, l: l, sessions: newSessionCheck(s, b, d, l),
		pending: map[string]acpPending{}, created: map[string]bool{}}
}

func (g *acpGuard) run() {
	var loops sync.WaitGroup
	loops.Go(func() { g.loop(g.l.down, g.fromDevice, true) })
	loops.Go(func() { g.loop(g.l.up, g.fromAgent, false) })
	g.l.run()
	loops.Wait()
	g.work.Wait()
}

func (g *acpGuard) loop(src *websocket.Conn, handle func(websocket.MessageType, []byte), fromDevice bool) {
	for {
		typ, data, err := src.Read(g.l.ctx)
		if err != nil {
			g.l.ended(err, fromDevice)
			return
		}
		handle(typ, data)
	}
}

func (g *acpGuard) toDevice(data []byte) {
	if err := g.l.down.Write(g.l.ctx, websocket.MessageText, data); err != nil {
		g.s.log.Debug("write to device failed", "device", g.dev.ID, "error", err.Error())
	}
}

func (g *acpGuard) toAgent(data []byte) {
	if err := g.l.up.Write(g.l.ctx, websocket.MessageText, data); err != nil {
		g.s.log.Debug("write to acp-host failed", "device", g.dev.ID, "error", err.Error())
	}
}

func hasIDMember(f *frame) bool {
	_, ok := f.members["id"]
	return ok
}

func acpDenied(id json.RawMessage, method string) []byte {
	return answerError(id, codeDenied, method+" is not available through hermes-hub", map[string]any{keyReason: reasonDenied})
}

// fromDevice applies the rules to one device message.
func (g *acpGuard) fromDevice(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText {
		g.s.log.Warn("dropped a binary message from a device", "leg", g.l.name, "device", g.dev.ID)
		return
	}
	f, err := parseFrame(data)
	if err != nil {
		code := codeInvalid
		if !json.Valid(data) {
			code = codeParse
		}
		g.toDevice(answerError(nil, code, "hermes-hub takes one JSON-RPC object per message", nil))
		return
	}
	notification := !hasIDMember(&f)
	if _, own := ownID(f.ID); own {
		if f.Method != "" && !notification {
			g.toDevice(answerError(f.ID, codeInvalid, "ids starting with "+OwnIDPrefix+" are the hub's", nil))
		}
		return
	}
	if f.Method == "" {
		if !notification {
			g.toAgent(data)
		}
		return
	}
	rule, ok := acpEditorRules[f.Method]
	if !ok || rule.notification != notification {
		g.s.log.Warn("refused a device ACP message", "device", g.dev.ID, "method", f.Method)
		if !notification {
			g.toDevice(acpDenied(f.ID, f.Method))
		}
		return
	}
	if !notification {
		if !validID(f.ID) {
			g.toDevice(answerError(nil, codeInvalid, "a request id must be a string or a number", nil))
			return
		}
	}
	params, isObject := decodeParams(f.Params)
	if err := g.check(f.Method, rule, params, isObject); err != nil {
		g.refuse(&f, notification, err)
		return
	}
	if notification {
		g.forward(&f, params)
		return
	}
	g.mu.Lock()
	g.nextOwn++
	own := fmt.Sprintf("%s%d", acpOwnPrefix, g.nextOwn)
	g.pending[own] = acpPending{id: f.ID, method: f.Method}
	g.mu.Unlock()
	f.members["id"] = mustRaw(own)
	g.forward(&f, params)
}

func (g *acpGuard) forward(f *frame, params map[string]json.RawMessage) {
	out, err := f.encode(params)
	if err != nil {
		return
	}
	g.toAgent(out)
}

// acpError is a device message the hub answers with an error.
type acpError struct {
	code    int
	message string
	reason  string
	err     error
}

func (e *acpError) Error() string { return e.message }

func (e *acpError) Unwrap() error { return e.err }

func invalidParams(message string) *acpError {
	return &acpError{code: codeInvalidParams, message: message}
}

// check applies the session and params rules to a message and rewrites its
// params as acp-host does.
func (g *acpGuard) check(method string, rule acpRule, params map[string]json.RawMessage, isObject bool) error {
	if !isObject {
		return invalidParams("hermes-hub takes only object params")
	}
	for k := range params {
		if !slices.Contains(rule.params, k) && (!rule.session || (k != keyACPSession && k != keyACPAlias)) {
			return invalidParams("params member " + strconv.Quote(k) + " is not allowed on " + method)
		}
	}
	if hasMember(params, acpMetaField) {
		return invalidParams("a field_meta member is not allowed")
	}
	if rule.session {
		if err := g.checkSession(params); err != nil {
			return err
		}
	}
	switch method {
	case acpNew, acpLoad, acpResume, acpFork:
		ph, err := g.placeholder()
		if err != nil {
			return &acpError{code: codeRPCInternal, message: "the hub could not read the placeholder directory: " + err.Error(),
				reason: reasonBind, err: err}
		}
		params[acpCWD] = mustRaw(ph)
		params[acpMCP] = json.RawMessage("[]")
		delete(params, acpMCPField)
	case acpPrompt:
		if !promptTextAllowed(params) {
			return &acpError{code: codeInvalidParams, message: "the prompt is a slash command that is not available through hermes-hub",
				reason: reasonDenied}
		}
		return rewritePromptLinks(params)
	}
	return nil
}

// promptTextAllowed applies the gateway leg's slash allowlist to every text
// of a prompt: hermes acp joins the text blocks and sends a prompt that
// names a registered command to the gateway as slash.exec over its own
// connection (acp_adapter/gateway_server.py _run_slash), which the hub
// does not see. Each string under a text member, at any depth, is checked
// on its own, since the joined text starts with one of them.
func promptTextAllowed(params map[string]json.RawMessage) bool {
	var blocks any
	if json.Unmarshal(params["prompt"], &blocks) != nil {
		return true
	}
	ok := true
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				if s, isString := c.(string); isString && k == keyText && !acpTextAllowed(s) {
					ok = false
				}
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(blocks)
	return ok
}

func (g *acpGuard) placeholder() (string, error) {
	ctx, cancel := context.WithTimeout(g.l.ctx, guardTimeout)
	defer cancel()
	return g.s.placeholderDir(ctx, g.b)
}

// hasMember reports whether any object in raw JSON, at any depth, has a
// member named name.
func hasMember(params map[string]json.RawMessage, name string) bool {
	var walk func(v any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				if k == name || walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	for k, raw := range params {
		var v any
		if k == name || (json.Unmarshal(raw, &v) == nil && walk(v)) {
			return true
		}
	}
	return false
}

// checkSession refuses a session of another device, and a session bound
// to no device unless acp-host created it for this device on this leg.
// Both sessionId and its alias session_id are checked, as the ACP SDK
// accepts either.
func (g *acpGuard) checkSession(params map[string]json.RawMessage) error {
	var sids []string
	for _, k := range []string{keyACPSession, keyACPAlias} {
		raw, ok := params[k]
		if !ok {
			continue
		}
		sid, isString := rawStringOK(raw)
		if !isString || sid == "" {
			return invalidParams(k + " must be a non-empty string")
		}
		sids = append(sids, sid)
	}
	if len(sids) == 0 {
		return invalidParams(keyACPSession + " is required")
	}
	for _, sid := range sids {
		g.mu.Lock()
		created := g.created[sid]
		g.mu.Unlock()
		err := g.sessions.check(sid, !created)
		switch {
		case errors.Is(err, errOtherDevice):
			return &acpError{code: codeInvalidParams, message: "session is bound to another device", reason: reasonOtherDev, err: err}
		case errors.Is(err, errNotBound):
			return &acpError{code: codeInvalidParams, message: "session " + sid + " is not bound to this device: open it from the editor through hermes-device acp",
				reason: reasonNotBound, err: err}
		case err != nil:
			return &acpError{code: codeRPCInternal, message: "the hub could not check the session's binding: " + err.Error(), reason: reasonBind, err: err}
		}
	}
	return nil
}

// acpSessionCommands are the slash commands hermes acp answers for the
// session itself (acp_adapter/gateway_commands.py ROUTES), as acp-host
// allows them: /discard resolves a lost turn of the session, which every
// later prompt waits for after a gateway restart, and /tools reads
// session.info.
func acpSessionCommands() []string {
	return []string{"discard", "tools"}
}

// acpTextAllowed is textAllowed plus acpSessionCommands. The exception
// takes only the form hermes acp itself reads as the command: the text
// starts with it, and its word is ASCII with no @bot suffix. Other forms
// (a leading @ line, /discard@x) reach the gateway as message text, which
// the gateway may read as its own command.
func acpTextAllowed(text string) bool {
	t := strings.TrimLeftFunc(text, pySpace)
	if word, ok := strings.CutPrefix(t, "/"); ok {
		arg := ""
		if end := strings.IndexFunc(word, pySpace); end >= 0 {
			word, arg = word[:end], strings.TrimLeftFunc(word[end:], pySpace)
		}
		if asciiWord(word) && slices.Contains(acpSessionCommands(), strings.ToLower(word)) {
			return !strings.Contains(flagForm(arg), flagGlobal)
		}
	}
	return textAllowed(map[string]json.RawMessage{keyText: mustRaw(text)})
}

func asciiWord(w string) bool {
	for _, r := range w {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// rewritePromptLinks replaces every resource_link block of a prompt with a
// text block, as acp-host does: hermes acp reads a link as a pod path.
func rewritePromptLinks(params map[string]json.RawMessage) error {
	raw, ok := params["prompt"]
	if !ok {
		return nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return invalidParams("prompt must be an array")
	}
	for i, b := range blocks {
		blk, isObj := decodeParams(b)
		if !isObj || rawString(blk["type"]) != "resource_link" {
			continue
		}
		blocks[i] = mustRaw(map[string]string{
			"type": "text",
			"text": fmt.Sprintf("[hermes-device: link %s removed; the bridge inlines device files]", rawString(blk["uri"])),
		})
	}
	params["prompt"] = mustRaw(blocks)
	return nil
}

func (g *acpGuard) refuse(f *frame, notification bool, err error) {
	var r *acpError
	if !errors.As(err, &r) {
		r = &acpError{code: codeRPCInternal, message: err.Error()}
	}
	g.s.log.Warn("refused a device ACP message", "device", g.dev.ID, "method", f.Method, "error", r.message)
	if notification {
		return
	}
	var data map[string]any
	if r.reason != "" {
		data = map[string]any{keyReason: r.reason}
	}
	g.toDevice(answerError(f.ID, r.code, r.message, data))
}

// fromAgent relays the agent's requests and notifications that Hermes
// sends, answers other requests itself, and gives each answer to a device
// request the device's id back.
func (g *acpGuard) fromAgent(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText {
		g.s.log.Warn("dropped a binary message from acp-host", "device", g.dev.ID)
		return
	}
	f, err := parseFrame(data)
	if err != nil {
		g.s.log.Warn("dropped a message from acp-host that is not one JSON-RPC object", "device", g.dev.ID)
		return
	}
	notification := !hasIDMember(&f)
	if f.Method != "" {
		rule, ok := acpAgentRules[f.Method]
		switch {
		case ok && rule.notification == notification:
			g.toDevice(data)
		case notification:
			g.s.log.Warn("dropped an ACP notification from acp-host", "device", g.dev.ID, "method", f.Method)
		default:
			g.s.log.Warn("refused an ACP request from acp-host to the device", "device", g.dev.ID, "method", f.Method)
			g.toAgent(acpDenied(f.ID, f.Method))
		}
		return
	}
	own, ok := ownID(f.ID)
	if !ok {
		if !f.hasID() && f.isError() {
			g.toDevice(data)
			return
		}
		g.s.log.Warn("dropped an ACP answer from acp-host to no request of the device", "device", g.dev.ID)
		return
	}
	g.mu.Lock()
	p, found := g.pending[own]
	delete(g.pending, own)
	g.mu.Unlock()
	if !found {
		return
	}
	f.members["id"] = p.id
	switch p.method {
	case acpNew, acpFork:
		if !f.isError() {
			var res struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(f.Result, &res) == nil && res.SessionID != "" {
				g.mu.Lock()
				g.created[res.SessionID] = true
				g.mu.Unlock()
				g.s.noteCreated(g.dev.ID, res.SessionID)
			}
		}
	case acpList:
		if !f.isError() {
			g.work.Go(func() { g.listAnswered(&f) })
			return
		}
	}
	if out, err := json.Marshal(f.members); err == nil {
		g.toDevice(out)
	}
}

// listAnswered keeps only the rows of sessions bound to this device, as
// acp-host does. When the bindings cannot be read, it keeps none.
func (g *acpGuard) listAnswered(f *frame) {
	ctx, cancel := context.WithTimeout(g.l.ctx, guardTimeout)
	bindings, err := g.b.plugin.Bindings(ctx)
	cancel()
	if err != nil {
		g.s.log.Warn("could not read bindings for an ACP session list; listing no sessions", "device", g.dev.ID, "error", err.Error())
	}
	mine := map[string]bool{}
	for _, b := range bindings {
		if b.DeviceID == g.dev.ID {
			mine[b.SessionID] = true
		}
	}
	res, isObj := decodeParams(f.Result)
	if !isObj {
		res = map[string]json.RawMessage{}
	}
	var rows []json.RawMessage
	_ = json.Unmarshal(res["sessions"], &rows)
	kept := []json.RawMessage{}
	for _, row := range rows {
		r, _ := decodeParams(row)
		if sid, ok := rawStringOK(r[keyACPSession]); ok && mine[sid] {
			kept = append(kept, row)
		}
	}
	res["sessions"] = mustRaw(kept)
	f.members["result"] = mustRaw(res)
	if out, err := json.Marshal(f.members); err == nil {
		g.toDevice(out)
	}
}
