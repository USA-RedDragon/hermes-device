package splice

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
)

// JSON-RPC error codes and data.reason values of docs/protocol.md 5.6.
const (
	CodeDenied = -32601
	CodeDevice = 4001

	ReasonDenied = "hermes_device_denied"
	ReasonCwd    = "hermes_device_cwd"
	ReasonBypass = "hermes_device_bypass"
	ReasonBind   = "hermes_device_bind"
)

const (
	jsonRPCVersion = "2.0"
	keyReason      = "reason"
	keySessionID   = "session_id"
	keyError       = "error"
	hubUnreachable = "hub_unreachable"
)

// OwnIDPrefix starts the id of every request the splice sends itself.
const OwnIDPrefix = "hermes-device-"

const (
	methodCreate       = "session.create"
	methodResume       = "session.resume"
	methodMutate       = "session.mutate"
	methodConfigSet    = "config.set"
	methodCompletePath = "complete.path"
	methodProcessList  = "process.list"
	methodWakeStart    = "wake.start"
	opWorkspace        = "workspace"
	opBranch           = "branch"
	opDelete           = "delete"
	opEditorMCP        = "editor_mcp"
)

// frame is one JSON-RPC message, decoded only as far as the splice needs.
type frame struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage
}

// parseFrame decodes a JSON object by exact member names. encoding/json
// matches struct fields case-insensitively, while the gateway (Python
// json) does not, so a struct decode could read a different "method" than
// the gateway acts on. With duplicate names both take the last one.
func parseFrame(data []byte) (frame, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return frame{}, err
	}
	if m == nil {
		return frame{}, errors.New("null message")
	}
	f := frame{ID: m["id"], Params: m["params"], Result: m["result"], Error: m["error"]}
	if raw, ok := m["method"]; ok {
		if err := json.Unmarshal(raw, &f.Method); err != nil {
			return frame{}, err
		}
	}
	return f, nil
}

func (f *frame) hasID() bool {
	return len(f.ID) > 0 && !isNull(f.ID)
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

// ownID reports whether raw is a string id the splice sent.
func ownID(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil || !strings.HasPrefix(s, OwnIDPrefix) {
		return "", false
	}
	return s, true
}

// rpcError is the error member of an answer the splice makes itself.
type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func answerError(id json.RawMessage, code int, message string, data map[string]any) []byte {
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
	return answerError(id, CodeDenied, method+" is not available through talaria",
		map[string]any{keyReason: ReasonDenied})
}

func cwdAnswer(id json.RawMessage, message string) []byte {
	return answerError(id, CodeDevice, message, map[string]any{keyReason: ReasonCwd})
}

func bindAnswer(id json.RawMessage, err error) []byte {
	return answerError(id, CodeDevice, "talaria could not bind the session to this device: "+err.Error(),
		map[string]any{keyReason: ReasonBind, "hub_error": hubErrorCode(err)})
}

func bypassAnswer(id json.RawMessage) []byte {
	return answerError(id, CodeDevice,
		"safe mode and ignore-user-config sessions run outside the device plugin, so talaria refuses them",
		map[string]any{keyReason: ReasonBypass})
}

// hubErrorCode is the hub's error code for err, or a code naming why there
// is none.
func hubErrorCode(err error) string {
	var he *hubapi.Error
	switch {
	case errors.As(err, &he):
		return he.Code
	case errors.Is(err, hubapi.ErrUnauthorized):
		return "unauthorized"
	default:
		return hubUnreachable
	}
}

// deniedPrefix reports whether method is in a family refused whole.
func deniedPrefix(method string) bool {
	for _, p := range []string{"vault.", "worker.", "connectors.", "oauth."} {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

// denied reports whether a request of method with params is refused
// (docs/protocol.md 5.4).
func denied(method string, params map[string]json.RawMessage) bool {
	switch method {
	case "model.save_key", "profiles.create", "shell.exec", "cli.exec", "image.attach", "input.detect_drop",
		"clipboard.paste", "mcp.servers.add", "mcp.servers.test", "mcp.servers.remove", "plugins.manage",
		"skills.manage", "cron.manage", "cron.submit", "kanban.run", "browser.manage", "display.install",
		"display.start", "a2a.forward", "process.kill", "rollback.restore", "set_api_key",
		"diagnostics.share_nous", "free_tier.provision", "session.foreign.import", "prompt.background":
		return true
	case methodConfigSet:
		return !configSetAllowed(params)
	case "pdf.attach", "file.attach":
		p, ok := params["path"]
		return ok && !isNull(p)
	case methodMutate:
		return rawString(params["operation"]) == opEditorMCP
	}
	return deniedPrefix(method)
}

// configSetAllowed reports whether a config.set changes only its session,
// as the hub decides (docs/protocol.md 5.4): busy, verbose, show_reasoning,
// service_tier, fast or yolo without a scope, or reasoning in session scope
// with exactly an effort level or reset. Every other key writes profile
// state of the pod.
func configSetAllowed(params map[string]json.RawMessage) bool {
	var key string
	if json.Unmarshal(params["key"], &key) != nil {
		return false
	}
	_, hasScope := params["scope"]
	switch key {
	case "busy", "verbose", "show_reasoning", "service_tier", "fast", "yolo":
		return !hasScope
	case "reasoning":
		if hasScope && rawString(params["scope"]) != "session" {
			return false
		}
		v := rawString(params["value"])
		return v == "reset" || slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, v)
	}
	return false
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// isTrue reports whether raw is JSON true.
func isTrue(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("true"))
}

// dropMember removes name from the object at params[key], if both exist.
func dropMember(params map[string]json.RawMessage, key, name string) {
	raw, ok := params[key]
	if !ok {
		return
	}
	obj, isObj := decodeParams(raw)
	if !isObj || isNull(raw) {
		return
	}
	if _, ok := obj[name]; !ok {
		return
	}
	delete(obj, name)
	params[key] = mustRaw(obj)
}

// decodeParams decodes params as an object. Absent params give an empty map.
func decodeParams(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	m := map[string]json.RawMessage{}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return m, true
	}
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]json.RawMessage{}, false
	}
	return m, true
}

// rawString decodes raw as a string, or returns "".
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// mustRaw encodes v, or returns nil when v cannot be encoded.
func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// withParams re-encodes the message data with params replaced, keeping
// every other member as sent.
func withParams(data []byte, params map[string]json.RawMessage) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	msg["params"] = p
	return json.Marshal(msg)
}
