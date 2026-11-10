package acphost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ACP methods acp-host rewrites.
const (
	methodNew    = "session/new"
	methodLoad   = "session/load"
	methodResume = "session/resume"
	methodFork   = "session/fork"
	methodList   = "session/list"
	methodPrompt = "session/prompt"
)

// Params members acp-host reads or writes.
const (
	blockText         = "text"
	keySessionID      = "sessionId"
	keySessionIDField = "session_id"
	keyMCPServers     = "mcpServers"
)

// metaKeys are the params members the ACP SDK merges into a handler's
// keyword arguments (agent-client-protocol 0.9.0, acp/router.py
// _make_func and acp/utils.py model_to_kwargs): the alias _meta and the
// field name field_meta, which the SDK also accepts. A member there named
// like a handler argument, such as session_id or cwd, replaces it.
func metaKeys() []string { return []string{"_meta", "field_meta"} }

// paramKeys lists, for each ACP method the SDK routes to Hermes
// (acp/agent/router.py), the params members of its request model
// (acp/schema.py), as alias and field name. The SDK accepts either name
// (populate_by_name); acp-host keeps only these members, under the alias.
func paramKeys(method string) (map[string]string, bool) {
	const cwd = "cwd"
	keys := func(withSession bool, pairs ...string) map[string]string {
		m := map[string]string{}
		if withSession {
			m[keySessionID] = keySessionIDField
		}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return m
	}
	switch method {
	case "initialize":
		return keys(false, "protocolVersion", "protocol_version", "clientCapabilities", "client_capabilities",
			"clientInfo", "client_info"), true
	case "authenticate":
		return keys(false, "methodId", "method_id"), true
	case methodNew:
		return keys(false, cwd, cwd, keyMCPServers, "mcp_servers"), true
	case methodLoad, methodResume, methodFork:
		return keys(true, cwd, cwd, keyMCPServers, "mcp_servers"), true
	case methodList:
		return keys(false, "cursor", "cursor", cwd, cwd), true
	case "session/close", "session/cancel":
		return keys(true), true
	case "session/set_mode":
		return keys(true, "modeId", "mode_id"), true
	case "session/set_model":
		return keys(true, "modelId", "model_id"), true
	case "session/set_config_option":
		return keys(true, "configId", "config_id", "type", "type", "value", "value"), true
	case methodPrompt:
		return keys(true, "messageId", "message_id", "prompt", "prompt"), true
	}
	return nil, false
}

// JSON-RPC error codes acp-host answers with itself.
const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeInvalidParams = -32602
	codeInternal      = -32603
)

type object = map[string]json.RawMessage

// rpcError is a JSON-RPC error that acp-host answers the editor with
// instead of forwarding the message.
type rpcError struct {
	id      json.RawMessage
	code    int
	message string
	// notify is set for a refused notification, which gets no answer.
	notify bool
}

func (e *rpcError) Error() string { return e.message }

func (e *rpcError) response() []byte {
	id := e.id
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": e.code, "message": e.message},
	})
	if err != nil {
		return nil
	}
	return b
}

// listRequest remembers a session/list request so its response can be
// filtered.
type listRequest struct {
	id  json.RawMessage
	dir string
}

// rewriter applies the section 6.2 rewrites for one connection.
type rewriter struct {
	placeholder  string
	bindingsFile string
	deviceID     string
	// lists holds the session/list requests in flight, by the id acp-host
	// gave them. The caller holds a lock.
	lists    map[string]listRequest
	nextList uint64
}

// isNotification reports whether m has no id member. Python's JSON-RPC
// treats "id": null as a request, so only a missing id is a notification.
func isNotification(m object) bool {
	_, ok := m["id"]
	return !ok
}

// fromEditor turns one WebSocket message into one stdin line. It returns a
// *rpcError when the message must be answered instead of forwarded.
func (r *rewriter) fromEditor(data []byte) ([]byte, error) {
	var m object
	if err := json.Unmarshal(data, &m); err != nil {
		if json.Valid(data) {
			return nil, &rpcError{code: codeInvalidReq, message: "hermes-device acp-host: a message must be one JSON-RPC object"}
		}
		return nil, &rpcError{code: codeParse, message: "hermes-device acp-host: parse error"}
	}
	var method string
	if raw, ok := m["method"]; ok {
		if err := json.Unmarshal(raw, &method); err != nil {
			return nil, &rpcError{id: m["id"], code: codeInvalidReq, message: "hermes-device acp-host: method must be a string", notify: isNotification(m)}
		}
	}
	if method != "" {
		if err := r.pinParams(m, method); err != nil {
			return nil, err
		}
	}
	switch method {
	case methodNew, methodLoad, methodResume, methodFork:
		if err := r.rewriteParams(m, func(p object) error {
			p["cwd"] = mustJSON(r.placeholder)
			p[keyMCPServers] = json.RawMessage("[]")
			return nil
		}); err != nil {
			return nil, err
		}
	case methodList:
		var dir string
		if err := r.rewriteParams(m, func(p object) error {
			raw, ok := p["cwd"]
			delete(p, "cwd")
			if !ok || string(raw) == "null" {
				return nil
			}
			if err := json.Unmarshal(raw, &dir); err != nil || !path.IsAbs(dir) {
				return errors.New("cwd must be an absolute path")
			}
			dir = path.Clean(dir)
			return nil
		}); err != nil {
			return nil, err
		}
		if id, ok := m["id"]; ok {
			r.nextList++
			own := fmt.Sprintf("hermes-device-list-%d", r.nextList)
			r.lists[own] = listRequest{id: id, dir: dir}
			m["id"] = mustJSON(own)
		}
	case methodPrompt:
		if err := r.rewriteParams(m, rewritePrompt); err != nil {
			return nil, err
		}
	}
	return json.Marshal(m)
}

// rewriteParams applies fn to m's params object. Params that are missing
// are treated as an empty object; params of another type are refused,
// since they could carry values past the rewrite.
func (r *rewriter) rewriteParams(m object, fn func(object) error) error {
	p := object{}
	if raw, ok := m["params"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return &rpcError{id: m["id"], code: codeInvalidParams, message: "hermes-device acp-host: params must be an object", notify: isNotification(m)}
		}
	}
	if err := fn(p); err != nil {
		e := &rpcError{id: m["id"], code: codeInvalidParams, message: "hermes-device acp-host: " + err.Error(), notify: isNotification(m)}
		switch {
		case errors.Is(err, errOtherDevice), errors.Is(err, errNotBound):
			e.message = err.Error()
		case errors.Is(err, errBindings):
			e.code, e.message = codeInternal, err.Error()
		}
		return e
	}
	m["params"] = mustJSON(p)
	return nil
}

// errOtherDevice refuses a session bound to a device other than the
// connection's.
var errOtherDevice = errors.New("session is bound to another device")

// errNotBound refuses a session that has no binding of its own.
var errNotBound = errors.New("session is not bound to this device")

// pinParams applies the params rules every editor request meets, before
// the per-method rewrites. For a method the SDK routes, only the members of
// its request model pass, each under its alias (a field name folds into
// the alias; with both, the alias wins, as in the SDK), so no _meta
// reaches the handler. Any other method loses the _meta members of object
// params, and its other params pass unchanged. Then every session id in
// params is checked.
func (r *rewriter) pinParams(m object, method string) error {
	keys, known := paramKeys(method)
	if !known && !isObject(m["params"]) {
		return nil
	}
	return r.rewriteParams(m, func(p object) error {
		if known {
			pinned := object{}
			for alias, field := range keys {
				if v, ok := p[alias]; ok {
					pinned[alias] = v
				} else if v, ok := p[field]; ok {
					pinned[alias] = v
				}
			}
			clear(p)
			maps.Copy(p, pinned)
		} else {
			for _, k := range metaKeys() {
				delete(p, k)
			}
		}
		for _, k := range []string{keySessionID, keySessionIDField} {
			if err := r.checkSession(p[k], len(p[k]) > 0); err != nil {
				return err
			}
		}
		return nil
	})
}

// isObject reports whether raw is a JSON object.
func isObject(raw json.RawMessage) bool {
	var p object
	return json.Unmarshal(raw, &p) == nil && p != nil
}

// checkSession refuses a session id that is not bound to the connection's
// device. Only the session's own binding counts: acp-host cannot read the
// lineage, and the bridge binds every session before it uses it (6.3).
func (r *rewriter) checkSession(raw json.RawMessage, present bool) error {
	if !present {
		return nil
	}
	var sid string
	if err := json.Unmarshal(raw, &sid); err != nil {
		return errors.New("sessionId must be a string")
	}
	bindings, err := readBindings(r.bindingsFile)
	if err != nil {
		return errBindings
	}
	b, ok := bindings[sid]
	switch {
	case !ok:
		return errNotBound
	case b.DeviceID != r.deviceID:
		return errOtherDevice
	}
	return nil
}

// errBindings refuses a session call when the bindings file cannot be read,
// so a broken file never lets a call through.
var errBindings = errors.New("hermes-device acp-host: cannot read the bindings file")

func rewritePrompt(p object) error {
	raw, ok := p["prompt"]
	if !ok {
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return errors.New("prompt must be an array")
	}
	for i, b := range blocks {
		var blk object
		if json.Unmarshal(b, &blk) != nil {
			continue
		}
		var typ string
		if json.Unmarshal(blk["type"], &typ) != nil || typ != "resource_link" {
			continue
		}
		var uri string
		_ = json.Unmarshal(blk["uri"], &uri)
		blocks[i] = mustJSON(map[string]string{
			"type":    blockText,
			blockText: fmt.Sprintf("[hermes-device: link %s removed; the bridge inlines device files]", uri),
		})
	}
	p["prompt"] = mustJSON(blocks)
	return checkSlash(blocks)
}

// slashCommands are the slash commands a prompt may run: the gateway
// leg's allowlist (docs/protocol.md 5.4), plus discard and tools, which
// hermes acp answers for the session itself (acp_adapter/gateway_commands.py
// ROUTES).
func slashCommands() []string {
	return []string{"new", "clear", "redraw", "history", "retry", "undo", "title", "compress", "stop", "status",
		"context", "usage", "help", "commands", "version", "copy", "queue", "steer", "btw", "branch", "diff",
		"verbose", "fast", "yolo", "reasoning", "model", "goal", "plan", "review", "refine", "whoami",
		"discard", "tools"}
}

func reasoningEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

// isPySpace reports whether Python's str.isspace holds for r, which
// unicode.IsSpace misses only for U+001C to U+001F.
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// checkSlash refuses a prompt that would run a slash command outside
// slashCommands. hermes acp runs a prompt whose text starts with a
// registered command as that command, and sends the ones it does not
// answer itself to the gateway's slash.exec (acp_adapter/gateway_server.py
// _slash_command and _run_slash), so without this check the ACP leg would
// reach every command that the hub refuses on the gateway leg, /device
// among them. The text is the text members of the blocks joined with
// newlines, as acp_adapter/content.py joins them; other blocks only add
// text that starts with "[", so this finds every command Hermes finds,
// and refuses a few prompts Hermes would treat as text.
func checkSlash(blocks []json.RawMessage) error {
	var texts []string
	for _, b := range blocks {
		var blk object
		var text string
		if json.Unmarshal(b, &blk) == nil && json.Unmarshal(blk[blockText], &text) == nil && text != "" {
			texts = append(texts, text)
		}
	}
	rest, ok := strings.CutPrefix(strings.TrimLeftFunc(strings.Join(texts, "\n"), isPySpace), "/")
	if !ok {
		return nil
	}
	name, arg := rest, ""
	if i := strings.IndexFunc(rest, isPySpace); i >= 0 {
		name, arg = rest[:i], strings.TrimFunc(rest[i:], isPySpace)
	}
	if name == "" || strings.Contains(name, "/") {
		return nil
	}
	lower := strings.ToLower(name)
	allowed := isASCII(name) && slices.Contains(slashCommands(), lower) && !strings.Contains(arg, "--global")
	if allowed && lower == "reasoning" {
		allowed = arg == "" || arg == "reset" || slices.Contains(reasoningEfforts(), arg)
	}
	if !allowed {
		return fmt.Errorf("/%s is not available through hermes-device", name)
	}
	return nil
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// fromAgent rewrites one stdout line. Only responses to tracked
// session/list requests change.
func (r *rewriter) fromAgent(line []byte) ([]byte, error) {
	if len(r.lists) == 0 {
		return line, nil
	}
	var m object
	if err := json.Unmarshal(line, &m); err != nil {
		return line, nil //nolint:nilerr // a line that is not JSON passes through unchanged
	}
	if _, isReq := m["method"]; isReq {
		return line, nil
	}
	var key string
	if json.Unmarshal(m["id"], &key) != nil {
		return line, nil //nolint:nilerr // only string ids are tracked list requests; others pass through
	}
	req, ok := r.lists[key]
	if !ok {
		return line, nil
	}
	delete(r.lists, key)
	m["id"] = req.id
	raw, ok := m["result"]
	if !ok {
		return json.Marshal(m)
	}
	var res object
	if err := json.Unmarshal(raw, &res); err != nil {
		return line, nil //nolint:nilerr // a result that is not an object passes through unchanged
	}
	var rows []object
	_ = json.Unmarshal(res["sessions"], &rows)
	bindings, err := readBindings(r.bindingsFile)
	kept := []object{}
	for _, row := range rows {
		var sid string
		if json.Unmarshal(row[keySessionID], &sid) != nil {
			continue
		}
		b, ok := bindings[sid]
		if !ok || b.DeviceID != r.deviceID || !within(b.Dir, req.dir) {
			continue
		}
		row["cwd"] = mustJSON(b.Dir)
		kept = append(kept, row)
	}
	res["sessions"] = mustJSON(kept)
	m["result"] = mustJSON(res)
	out, merr := json.Marshal(m)
	if merr != nil {
		return nil, merr
	}
	return out, err
}

// within reports whether dir is base or below it. An empty base matches
// every dir.
func within(dir, base string) bool {
	if base == "" {
		return true
	}
	dir = path.Clean(dir)
	return dir == base || base == "/" || strings.HasPrefix(dir, base+"/")
}

// Binding is one entry of the plugin's bindings file (docs/protocol.md
// section 8.2).
type Binding struct {
	DeviceID string `json:"device_id"`
	Dir      string `json:"dir"`
}

type bindingsFile struct {
	Bindings map[string]Binding `json:"bindings"`
}

// readBindings reads the bindings file. A missing file has no bindings.
// On any other error it returns no bindings and the error, so callers
// fail closed.
func readBindings(file string) (map[string]Binding, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Binding{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f bindingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return f.Bindings, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
