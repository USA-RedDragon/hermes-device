package device_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const (
	keyTitle         = "title"
	keySource        = "source"
	keyPath          = "path"
	keyCols          = "cols"
	keyResult        = "result"
	keyProfile       = "profile"
	acpNew           = "session/new"
	acpFork          = "session/fork"
	podDir           = "/opt/data"
	methodSpawnList  = "spawn_tree.list"
	methodConfigGet  = "config.get"
	nameOther        = "other"
	titleOther       = "Other"
	keyMeta          = "_meta"
	methodFileAttach = "file.attach"
	keySubmission    = "submission_id"
	acpResume        = "session/resume"
	acpSetMode       = "session/set_mode"
	keyProject       = "project"
	methodList       = "session.list"
	sessionGone      = "s-gone"
	methodCreate     = "session.create"
	codeInvalid      = -32600
	codeBadParams    = -32602
)

// TestACPLegMeta covers the ACP SDK merging params._meta (and field_meta)
// into the handler's arguments, where _meta.session_id or _meta.cwd
// replaced the members the hub checked.
func TestACPLegMeta(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		e.plugin.SetBinding(sessionOther, otherDevice, "/home/bob")
		a := e.acpLeg(tok)
		n := len(e.acp.Received())
		prompt := []any{map[string]any{keyType: keyText, keyText: "hi"}}
		for _, tc := range []struct {
			method string
			params map[string]any
		}{
			{acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: prompt, keyMeta: map[string]any{keySessionID: sessionOther}}},
			{acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: prompt, keyMeta: map[string]any{keySessionID: sessionUnbound}}},
			{acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: prompt, "field_meta": map[string]any{keySessionID: sessionOther}}},
			{acpLoad, map[string]any{keySessionACP: sessionMine, keyCWD: deviceDir, keyMeta: map[string]any{keySessionID: sessionOther}}},
			{acpResume, map[string]any{keySessionACP: sessionMine, keyCWD: deviceDir, keyMeta: map[string]any{keySessionID: sessionOther}}},
			{acpFork, map[string]any{keySessionACP: sessionMine, keyCWD: deviceDir, keyMeta: map[string]any{keySessionID: sessionOther}}},
			{"session/set_model", map[string]any{keySessionACP: sessionMine, "modelId": "x", keyMeta: map[string]any{keySessionID: sessionOther}}},
			{acpSetMode, map[string]any{keySessionACP: sessionMine, "modeId": "x", keyMeta: map[string]any{keySessionID: sessionOther}}},
			{"session/set_config_option", map[string]any{keySessionACP: sessionMine, "configId": "x", keyValue: "y",
				keyMeta: map[string]any{keySessionID: sessionOther}}},
			{acpNew, map[string]any{keyCWD: deviceDir, keyMCPServers: []any{}, keyMeta: map[string]any{keyCWD: podDir}}},
			{acpLoad, map[string]any{keySessionACP: sessionMine, keyCWD: deviceDir, keyMeta: map[string]any{keyCWD: podDir}}},
			{acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: prompt, "workspace": podDir}},
			{acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: []any{map[string]any{keyType: keyText, keyText: "hi",
				"field_meta": map[string]any{}}}}},
			{acpInitialize, map[string]any{"protocolVersion": 1, keyMeta: map[string]any{}}},
		} {
			if m := a.request(tc.method, tc.params); errCode(m) != codeBadParams {
				t.Fatalf("%s %v = %v", tc.method, tc.params, m)
			}
		}
		writeJSON(t, a.c, map[string]any{keyJSONRPC: jsonRPC, keyMethod: acpCancel, keyParams: map[string]any{keySessionACP: sessionMine,
			keyMeta: map[string]any{keySessionID: sessionOther}}})
		a.request(acpInitialize, map[string]any{"clientCapabilities": map[string]any{keyMeta: map[string]any{"terminal_output": true}}})
		if got := e.acp.Received()[n:]; len(got) != 1 || got[0][keyMethod] != acpInitialize {
			t.Fatalf("refused messages reached the agent: %v", got)
		}
		for _, tc := range []struct {
			method string
			params map[string]any
		}{
			{acpNew, map[string]any{keyCWD: deviceDir}},
			{acpLoad, map[string]any{keySessionACP: sessionMine, keyCWD: podDir}},
			{acpResume, map[string]any{keySessionACP: sessionMine}},
			{acpFork, map[string]any{keySessionID: sessionMine, keyCWD: deviceDir}},
		} {
			if m := a.request(tc.method, tc.params); m[keyResult] == nil {
				t.Fatalf("%s = %v", tc.method, m)
			}
			sent := a.lastAgentMessage()
			if p, _ := sent[keyParams].(map[string]any); p[keyCWD] != placeholder {
				t.Fatalf("%s reached the agent as %v", tc.method, sent)
			}
		}
	})
}

