package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// JSON-RPC error codes and data.reason values of the gateway leg. They
// match the device splice's (docs/protocol.md 5.6), so the TUI shows a
// refusal by the hub as it shows one by the splice.
const (
	codeDenied        = -32601
	codeInvalid       = -32600
	codeInvalidParams = -32602
	codeDevice        = 4001
	reasonDenied      = "hermes_device_denied"
	reasonCwd         = "hermes_device_cwd"
	reasonBypass      = "hermes_device_bypass"
	reasonBind        = "hermes_device_bind"
	reasonOtherDev    = "hermes_device_other_device"
	reasonNotBound    = "hermes_device_not_bound"
	keyFast           = "fast"
	keyReasoning      = "reasoning"
	jsonRPCVersion    = "2.0"
	keyReason         = "reason"
	keySessionID      = "session_id"
	keyParentSession  = "parent_session_id"
)

// OwnIDPrefix starts the JSON-RPC id of every request the hub sends on a
// gateway leg itself. The hub refuses device requests with such an id.
const OwnIDPrefix = "hermes-hub-"

const (
	methodCreate       = "session.create"
	methodResume       = "session.resume"
	methodMutate       = "session.mutate"
	methodDetach       = "session.detach"
	methodConfigSet    = "config.set"
	methodCompletePath = "complete.path"
	opWorkspace        = "workspace"
	opBranch           = "branch"
	opDelete           = "delete"
	opEditorMCP        = "editor_mcp"
)

// frame is one JSON-RPC message, decoded by exact member names as Python's
// json does; encoding/json would match struct fields in any case.
type frame struct {
	members map[string]json.RawMessage
	ID      json.RawMessage
	Method  string
	Params  json.RawMessage
	Result  json.RawMessage
	Error   json.RawMessage
}

var errNotObject = errors.New("not a JSON object")

func parseFrame(data []byte) (frame, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return frame{}, err
	}
	if m == nil {
		return frame{}, errNotObject
	}
	f := frame{members: m, ID: m["id"], Params: m["params"], Result: m["result"], Error: m["error"]}
	if raw, ok := m["method"]; ok {
		if err := json.Unmarshal(raw, &f.Method); err != nil {
			return frame{}, err
		}
	}
	return f, nil
}

func (f *frame) hasID() bool { return len(f.ID) > 0 && !isNull(f.ID) }

func (f *frame) isError() bool { return len(f.Error) > 0 && !isNull(f.Error) }

// encode re-encodes the frame with params replaced, so the gateway reads
// exactly the members the hub checked.
func (f *frame) encode(params map[string]json.RawMessage) ([]byte, error) {
	m := make(map[string]json.RawMessage, len(f.members))
	for k, v := range f.members {
		m[k] = v
	}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		m["params"] = p
	}
	return json.Marshal(m)
}

// idKey turns a raw JSON id into a map key that is the same for every
// encoding of the same string or number.
func idKey(raw json.RawMessage) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return "", false
	}
	switch v := v.(type) {
	case string:
		return "s:" + v, true
	case json.Number:
		return "n:" + v.String(), true
	}
	return "", false
}

func ownID(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil || !strings.HasPrefix(s, OwnIDPrefix) {
		return "", false
	}
	return s, true
}

type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func answerError(id json.RawMessage, code int, message string, data map[string]any) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcError        `json:"error"`
	}{jsonRPCVersion, id, rpcError{Code: code, Message: message, Data: data}})
	if err != nil {
		return nil
	}
	return b
}

func answerResult(id json.RawMessage, result any) []byte {
	b, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{jsonRPCVersion, id, result})
	if err != nil {
		return nil
	}
	return b
}

func deniedAnswer(id json.RawMessage, method string) []byte {
	return answerError(id, codeDenied, method+" is not available through hermes-device", map[string]any{keyReason: reasonDenied})
}

func cwdAnswer(id json.RawMessage, message string) []byte {
	return answerError(id, codeDevice, message, map[string]any{keyReason: reasonCwd})
}

func bypassAnswer(id json.RawMessage) []byte {
	return answerError(id, codeDevice,
		"safe mode and ignore-user-config sessions run outside the device plugin, so hermes-device refuses them",
		map[string]any{keyReason: reasonBypass})
}

func bindAnswer(id json.RawMessage, err error) []byte {
	return answerError(id, codeDevice, "the hub could not bind the session to this device: "+err.Error(),
		map[string]any{keyReason: reasonBind, "hub_error": errorCode(err)})
}

func otherDeviceAnswer(id json.RawMessage, sessionID string) []byte {
	return answerError(id, codeDevice, "session "+sessionID+" is bound to another device",
		map[string]any{keyReason: reasonOtherDev})
}

func notBoundAnswer(id json.RawMessage, sessionID string) []byte {
	return answerError(id, codeDevice, "session "+sessionID+" is not bound to this device: resume it with hermes-device chat, which binds it first",
		map[string]any{keyReason: reasonNotBound})
}

// deniedMethods are refused whole: docs/protocol.md 5.4, plus the
// gateway's methods that stop pod cron jobs, prune sessions in bulk, and
// relay into the profile's messaging bots (gateway/session_controls.py
// handlers).
//
//nolint:gochecknoglobals // a fixed table
var deniedMethods = map[string]bool{
	"model.save_key": true, "profiles.create": true, "shell.exec": true, "cli.exec": true, "image.attach": true,
	"input.detect_drop": true, "clipboard.paste": true, "mcp.servers.add": true, "mcp.servers.test": true,
	"mcp.servers.remove": true, "plugins.manage": true, "skills.manage": true, "cron.manage": true, "cron.submit": true,
	"kanban.run": true, "browser.manage": true, "display.install": true, "display.start": true, "a2a.forward": true,
	"process.kill": true, "process.list": true, "rollback.restore": true, "set_api_key": true,
	"diagnostics.share_nous": true, "free_tier.provision": true, "session.foreign.import": true, "prompt.background": true,
	"cron.cancel": true, "cron.recover": true, "session.prune": true, "reload.mcp": true, "process.stop": true,
	"skills.reload": true, "llm.oneshot": true, "session.automation": true, "session.control": true,
	"connection.respond": true, "preview.restart": true,
}

// deniedPrefixes are method families refused whole.
//
//nolint:gochecknoglobals // a fixed table
var deniedPrefixes = []string{"vault.", "worker.", "connectors.", "oauth.", "bot_relay.", "groups.", "onboarding.", "rollback."}

// SessionConfigKeys are the config.set keys that change only the one
// session, so a device may set them. Every other key writes the profile's
// config.yaml or other profile-wide state of the pod and is refused. From
// Hermes v0.22.7 gateway/session_busy_controls.py (busy, verbose,
// show_reasoning, service_tier, yolo without scope) and
// gateway/session_tui_config.py (fast, and reasoning with scope session).
func SessionConfigKeys() []string {
	return append(scopelessKeys(), keyReasoning)
}

// scopelessKeys are the session config.set keys that take no scope.
func scopelessKeys() []string {
	return []string{"busy", "verbose", "show_reasoning", "service_tier", "yolo", keyFast}
}

// configSetAllowed reports whether a config.set changes only its session.
func configSetAllowed(params map[string]json.RawMessage) bool {
	key, ok := rawStringOK(params["key"])
	if !ok {
		return false
	}
	_, hasScope := params["scope"]
	if slices.Contains(scopelessKeys(), key) {
		return !hasScope
	}
	if key != keyReasoning || (hasScope && rawString(params["scope"]) != "session") {
		return false
	}
	return reasoningValue(rawString(params["value"]))
}

// reasoningValue reports whether v is exactly a reasoning effort or reset:
// a session setting. Anything else, such as show or hide with characters
// Python's strip removes, is refused rather than parsed as Hermes would.
func reasoningValue(v string) bool {
	return v == "reset" || slices.Contains(hub.ReasoningEfforts(), v)
}

// otherProfile reports whether a request names a profile: the gateway
// routes those to a sibling profile, which may not run device sessions.
func otherProfile(params map[string]json.RawMessage) bool {
	raw, ok := params["profile"]
	return ok && !isNull(raw) && rawString(raw) != ""
}

// SlashCommands are the slash commands a device may run through slash.exec
// and command.dispatch: ones that act on the session only, or only read.
// Any argument with --global is refused, and /reasoning takes only an
// effort or reset.
func SlashCommands() []string {
	return []string{"new", "clear", "redraw", "history", "retry", "undo", "title", "compress", "stop", "status",
		"context", "usage", "help", "commands", "version", "copy", "queue", "steer", "btw", "branch", "diff",
		"verbose", keyFast, "yolo", keyReasoning, "model", "goal", "plan", "review", "refine", "whoami"}
}

// slashAllowed applies SlashCommands to a slash.exec (command) or
// command.dispatch (name and arg) request, split as Hermes splits it
// (gateway/session_commands.py _parse).
func slashAllowed(method string, params map[string]json.RawMessage) bool {
	var name, arg string
	if method == "command.dispatch" {
		name, arg = rawString(params["name"]), rawString(params["arg"])
	} else {
		parts := strings.SplitN(strings.TrimLeft(strings.TrimSpace(rawString(params["command"])), "/"), " ", 2)
		name = parts[0]
		if len(parts) == 2 {
			arg = parts[1]
		}
	}
	name = strings.TrimLeft(strings.TrimSpace(name), "/")
	if !slices.Contains(SlashCommands(), name) || strings.Contains(arg, "--global") {
		return false
	}
	if name == keyReasoning {
		a := strings.TrimSpace(arg)
		return a == "" || reasoningValue(a)
	}
	return true
}

// denied reports whether a request of method with params is refused.
func denied(method string, params map[string]json.RawMessage) bool {
	if deniedMethods[method] {
		return true
	}
	switch method {
	case methodConfigSet:
		return !configSetAllowed(params)
	case "slash.exec", "command.dispatch":
		return !slashAllowed(method, params)
	case "pdf.attach", "file.attach":
		p, ok := params["path"]
		return ok && !isNull(p)
	case methodMutate:
		return rawString(params["operation"]) == opEditorMCP
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

func rewritten(method string) bool {
	return method == methodCreate || method == methodResume || method == methodMutate
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isTrue(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("true"))
}

func decodeParams(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	m := map[string]json.RawMessage{}
	if len(raw) == 0 || isNull(raw) {
		return m, true
	}
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]json.RawMessage{}, false
	}
	return m, true
}

func rawStringOK(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func rawString(raw json.RawMessage) string {
	s, _ := rawStringOK(raw)
	return s
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// dropMember removes name from the object at params[key], if both exist,
// and re-encodes that object.
func dropMember(params map[string]json.RawMessage, key, name string) {
	raw, ok := params[key]
	if !ok || isNull(raw) {
		return
	}
	obj, isObj := decodeParams(raw)
	if !isObj {
		return
	}
	delete(obj, name)
	params[key] = mustRaw(obj)
}

// absDir reports whether s is a clean enough absolute POSIX path to send
// to a device.
func absDir(s string) bool {
	return s != "" && path.IsAbs(s) && !strings.ContainsRune(s, 0)
}
