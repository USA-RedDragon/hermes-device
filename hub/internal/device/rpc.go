package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"slices"
	"strconv"
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
	codeParse         = -32700
	codeRPCInternal   = -32603
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
	keyCWD            = "cwd"
	keyRequestID      = "request_id"
	keyTitle          = "title"
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
	methodDispatch     = "command.dispatch"
	opWorkspace        = "workspace"
	opBranch           = "branch"
	opDelete           = "delete"
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

// validID reports whether a raw JSON id is a string or a number.
func validID(raw json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return false
	}
	switch v.(type) {
	case string, json.Number:
		return true
	}
	return false
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

func deniedAnswer(id json.RawMessage, method string, params map[string]json.RawMessage) []byte {
	why := DeniedReason(method)
	if _, ok := gatewayRules[method]; ok {
		why = "these params are refused"
		switch r := paramRefusal(method, params); {
		case r != "":
			why = r
		case slices.Contains(textMethods, method):
			why = "its text is a slash command that is not allowed: only " + strings.Join(SlashCommands(), ", ") +
				" (a /model needs --session)"
		case method == methodMutate:
			why = "operation " + strconv.Quote(rawString(params["operation"])) + " is refused"
		}
	}
	return answerError(id, codeDenied, method+" is not available through hermes-device: "+why, map[string]any{keyReason: reasonDenied})
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

// Gateway leg method rules: how the hub lets a method through.
const (
	// RuleAllowed passes unchanged.
	RuleAllowed = "allowed"
	// RuleRewritten passes after the hub rewrites it or holds its answer.
	RuleRewritten = "rewritten"
	// RuleChecked passes only when its params pass a check.
	RuleChecked = "checked"
	// RuleDenied is refused.
	RuleDenied = "denied"
)

// gatewayRules is the gateway leg allowlist: every method the Hermes
// v0.22.7 Ink TUI and classic CLI send for chatting, except those in
// deniedWhy. Every other method is refused. Session ids in session_id and
// parent_session_id are checked for every method.
//
//nolint:gochecknoglobals // a fixed table
var gatewayRules = map[string]string{
	"gateway.ping": RuleAllowed, "runtime.describe": RuleAllowed, "setup.status": RuleAllowed,
	methodCreate: RuleRewritten, methodResume: RuleRewritten, methodMutate: RuleRewritten, methodCompletePath: RuleRewritten,
	methodDetach: RuleAllowed, "session.list": RuleAllowed, "session.active_list": RuleAllowed, "session.most_recent": RuleAllowed,
	"session.info": RuleAllowed, "session.stats": RuleAllowed, "session.export": RuleAllowed,
	"session.context_breakdown": RuleAllowed, "session.control.read": RuleAllowed, "terminal.resize": RuleAllowed,
	"prompt.submit": RuleAllowed, "prompt.receipt": RuleAllowed, "prompt.cancel": RuleAllowed, "prompt.resolve_unknown": RuleAllowed,
	"prompt.replace_queued": RuleAllowed, "prompt.btw": RuleAllowed,
	"session.steer": RuleAllowed, "session.redirect": RuleAllowed, "session.interrupt": RuleAllowed,
	"approval.respond": RuleAllowed, "clarify.respond": RuleAllowed, "clarify.lock": RuleAllowed,
	methodConfigSet: RuleChecked, "config.get": RuleChecked, "model.options": RuleRewritten,
	methodSlashExec: RuleChecked, methodDispatch: RuleChecked, "command.observe": RuleAllowed, "commands.catalog": RuleAllowed,
	"complete.slash": RuleAllowed, "plugins.list": RuleAllowed, "tools.show": RuleAllowed,
	"subagent.list": RuleAllowed, "subagent.tail": RuleAllowed, "subagent.steer": RuleAllowed, "subagent.interrupt": RuleAllowed,
	"image.attach_bytes": RuleAllowed, "image.detach": RuleAllowed, methodFileAttach: RuleChecked,
	"learning.frames": RuleRewritten, "learning.detail": RuleAllowed, "pet.info.meta": RuleAllowed, "pet.cells": RuleRewritten,
	"pet.gallery": RuleAllowed, "i18n.catalog": RuleAllowed, "system.battery": RuleAllowed,
	"shared_metrics.slash_command": RuleAllowed, "shared_metrics.startup_latency": RuleAllowed,
}

// Reasons the hub refuses methods the TUI or CLI send.
const (
	whyPodExec    = "it runs programs in the Hermes pod, not on the device"
	whyPodFiles   = "it reads or writes files of the Hermes pod, not of the device"
	whyProfile    = "it changes settings or data of the whole Hermes profile"
	whyAccount    = "it acts on credentials or the account of the Hermes profile"
	whyHost       = "it uses the Hermes pod's own audio or display"
	whyLegacy     = "the gateway serves it only to legacy clients and refuses it on the hub's connection"
	whyOtherSess  = "it acts on or reads many sessions at once"
	whyBackground = "a background agent has no lineage to its session, so it would run on the pod"
	whyProfiles   = "it reads or changes other Hermes profiles"
	whyNotChat    = "it is not part of chatting"
)

// deniedWhy are the methods the Hermes v0.22.7 TUI or CLI send that the
// hub refuses, with the reason.
//
//nolint:gochecknoglobals // a fixed table
var deniedWhy = map[string]string{
	"shell.exec": whyPodExec, "process.list": whyPodExec, "process.stop": whyPodExec, "process.kill": whyPodExec,
	"reload.mcp": whyPodExec, "browser.manage": whyPodExec, "kanban.run": whyPodExec, "prompt.background": whyBackground,
	"session.save": whyPodFiles, "spawn_tree.save": whyPodFiles, "image.attach": whyPodFiles, "clipboard.paste": whyPodFiles,
	"input.detect_drop": whyPodFiles, "paste.collapse": whyPodFiles, "rollback.list": whyPodFiles, "rollback.diff": whyPodFiles,
	"rollback.restore": whyPodFiles, "rollback.session_diff": whyPodFiles,
	"tools.configure": whyProfile, "plugins.manage": whyProfile, "skills.manage": whyProfile, "skills.reload": whyProfile,
	"reload.env": whyProfile, "delegation.pause": whyProfile, "pet.select": whyProfile, "learning.edit": whyProfile,
	"learning.delete": whyProfile, "wake.start": whyHost, "wake.stop": whyHost, "wake.resume": whyHost, "wake.status": whyHost,
	"voice.toggle": whyHost, "voice.record": whyHost, "session.automation": whyProfile, "session.prune": whyOtherSess,
	"model.save_key": whyAccount, "model.disconnect": whyAccount, "connection.respond": whyAccount,
	"connectors.connect": whyAccount, "connectors.operation.status": whyAccount, "connectors.list": whyAccount,
	"connectors.accounts": whyAccount, "connectors.accounts.rename": whyAccount, "connectors.accounts.remove": whyAccount,
	"billing.state": whyAccount, "billing.step_up": whyAccount, "billing.charge": whyAccount, "billing.charge_status": whyAccount,
	"billing.auto_reload": whyAccount, "billing.keep_going_on_credits": whyAccount, "subscription.state": whyAccount,
	"subscription.preview": whyAccount, "subscription.change": whyAccount, "subscription.resume": whyAccount,
	"subscription.upgrade": whyAccount,
	"session.activate":     whyLegacy, "session.title": whyLegacy, "session.close": whyLegacy, "session.delete": whyLegacy,
	"session.branch": whyLegacy, "session.compress": whyLegacy, "session.undo": whyLegacy, "session.status": whyLegacy,
	"session.usage": whyLegacy, "session.archive": whyLegacy,
	"display.observe": whyHost, "free_tier.status": whyAccount, "onboarding.start": whyProfile, "onboarding.state": whyProfile,
	"profiles.describe": whyProfiles, "profiles.list": whyProfiles, "projects.tree": whyProfile,
	"setup.runtime_check": whyNotChat, "shared_metrics.update_run": whyNotChat,
	"delegation.status": whyOtherSess, "spawn_tree.list": whyOtherSess, "spawn_tree.load": whyOtherSess,
}

// whyUnknown is the reason for a method in neither table.
const whyUnknown = "the hub allows only the methods the Hermes TUI and CLI need for chatting"

// mutateOps are the session.mutate operations a device may run. The
// others are refused: editor_mcp spawns editor MCP programs on the pod,
// import reads other agents' histories from pod files.
//
//nolint:gochecknoglobals // a fixed table
var mutateOps = []string{"rename", "archive", "sidebar", "model", "reasoning", opWorkspace, "edit_policy", opDelete,
	"rewind", "reset", opBranch, "compress"}

// GatewayMethods are the methods the gateway leg lets through, in order.
func GatewayMethods() []string { return slices.Sorted(maps.Keys(gatewayRules)) }

// GatewayRule is how the gateway leg treats method: one of the Rule*
// values.
func GatewayRule(method string) string {
	if r, ok := gatewayRules[method]; ok {
		return r
	}
	return RuleDenied
}

// DeniedReason says why the gateway leg refuses method.
func DeniedReason(method string) string {
	if why, ok := deniedWhy[method]; ok {
		return why
	}
	return whyUnknown
}

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

// otherProfile reports whether a request has a non-null profile member,
// of any type: the gateway routes those to a sibling profile, which may
// not run device sessions.
func otherProfile(params map[string]json.RawMessage) bool {
	raw, ok := params["profile"]
	return ok && !isNull(raw)
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
	if method == methodDispatch {
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

// refusedParams are the params the hub refuses on methods it allows,
// with the reason.
//
//nolint:gochecknoglobals // a fixed table
var refusedParams = map[string]map[string]string{
	methodCreate: {
		keyTitle:       "a create with a title returns the existing session of that title or id",
		"base_url":     "it would send the profile's stored key to a host the device picks",
		"api_key":      "a launch key is not part of a device session",
		"accept_hooks": "it records shell hook consent for the whole profile",
	},
	methodResume: {
		keyTitle: "resume a session by its id: the hub checks the id before the gateway attaches it",
		"latest": "resume a session by its id: the hub checks the id before the gateway attaches it",
	},
	"config.get":     {keyCWD: "it runs git in a directory of the Hermes pod"},
	methodFileAttach: {"staged_upload": "it copies a browser upload out of the Hermes pod"},
}

// RefusedParams are the params the gateway leg refuses on each method it
// allows, with the reason.
func RefusedParams() map[string]map[string]string {
	out := make(map[string]map[string]string, len(refusedParams))
	for m, p := range refusedParams {
		out[m] = maps.Clone(p)
	}
	return out
}

// paramRefusal says why the params of an allowed method are refused, or
// is empty.
func paramRefusal(method string, params map[string]json.RawMessage) string {
	for _, k := range slices.Sorted(maps.Keys(refusedParams[method])) {
		if _, ok := params[k]; ok {
			return "param " + k + " is refused: " + refusedParams[method][k]
		}
	}
	if method == methodResume {
		if sid, ok := rawStringOK(params[keySessionID]); !ok || sid == "" {
			return "it needs a session_id: the hub checks the id before the gateway attaches the session"
		}
	}
	return ""
}

// denied reports whether a request of method with params is refused.
func denied(method string, params map[string]json.RawMessage) bool {
	if _, ok := gatewayRules[method]; !ok {
		return true
	}
	if paramRefusal(method, params) != "" || (slices.Contains(textMethods, method) && !textAllowed(params)) {
		return true
	}
	switch method {
	case methodConfigSet:
		return !configSetAllowed(params)
	case "slash.exec", methodDispatch:
		return !slashAllowed(method, params)
	case "file.attach":
		p, ok := params["path"]
		return ok && !isNull(p)
	case methodMutate:
		return !slices.Contains(mutateOps, rawString(params["operation"]))
	}
	return false
}

// sizeBounds are the bounds the hub clamps the size params of the TUI's
// render methods to: Hermes renders whatever size it is asked for.
//
//nolint:gochecknoglobals // a fixed table
var sizeBounds = map[string]map[string][2]float64{
	"learning.frames": {"cols": {20, 400}, "rows": {10, 200}, "frames": {1, 120}},
	"pet.cells":       {"cols": {4, 200}},
}

// rewriteParams applies the hub's param rewrites to an allowed method:
// it clamps render sizes, drops a model.options refresh (it refetches
// every provider's model list for the whole profile) and drops
// cwd_explicit from a create (false moves the session to the profile's
// workspace in the pod). It reports false for a size that is neither
// null nor a number.
func rewriteParams(method string, params map[string]json.RawMessage) bool {
	switch method {
	case methodSlashExec, methodDispatch:
		forceSessionModel(method, params)
	case "model.options":
		delete(params, "refresh")
	case methodCreate:
		delete(params, "cwd_explicit")
	}
	for k, b := range sizeBounds[method] {
		raw, ok := params[k]
		if !ok || isNull(raw) {
			continue
		}
		var n float64
		if json.Unmarshal(raw, &n) != nil {
			return false
		}
		params[k] = mustRaw(int(min(max(n, b[0]), b[1])))
	}
	return true
}

// ownRequestID reports whether a request_id param starts with the hub's
// own prefix, which the hub uses for its own requests.
func ownRequestID(params map[string]json.RawMessage) bool {
	s, ok := rawStringOK(params[keyRequestID])
	return ok && strings.HasPrefix(s, OwnIDPrefix)
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
