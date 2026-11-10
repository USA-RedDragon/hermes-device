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

// TestBindTakeover covers PUT /api/device/sessions/<child>/binding taking
// over a session that only inherits another device's binding, or that was
// bound before.
func TestBindTakeover(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.connectExec(e.a, tok)
		bind := func(sid string) (int, map[string]any) {
			t.Helper()
			return call(t, http.MethodPut, e.a.url+"/api/device/sessions/"+sid+"/binding", tok, map[string]any{keyDir: "/home/x"})
		}
		e.plugin.SetBinding(sessionOther, otherDevice, "/home/bob")
		e.plugin.SetParent("s-other-child", sessionOther)
		if s, out := bind("s-other-child"); s != http.StatusConflict || out["error"] != "bound_to_other_device" {
			t.Fatalf("bind of an inherited session = %d %v", s, out)
		}
		if s, out := bind(sessionOther); s != http.StatusConflict || out["error"] != "bound_to_other_device" {
			t.Fatalf("bind of another device's session = %d %v", s, out)
		}
		if _, ok := e.plugin.Bindings()["s-other-child"]; ok {
			t.Fatal("the inherited session was bound")
		}
		if s, out := bind(sessionGone); s != http.StatusOK || out[keyDeviceID] != id {
			t.Fatalf("bind of a never bound session = %d %v", s, out)
		}
		if s, _ := call(t, http.MethodDelete, e.b.url+"/api/device/sessions/"+sessionGone+"/binding", tok, nil); s != http.StatusNoContent {
			t.Fatalf("unbind = %d", s)
		}
		if s, out := bind(sessionGone); s != http.StatusConflict || out["error"] != "was_bound" {
			t.Fatalf("bind of a session bound before = %d %v", s, out)
		}
		e.plugin.SetParent("s-gone-child", sessionGone)
		if s, out := bind("s-gone-child"); s != http.StatusConflict || out["error"] != "was_bound" {
			t.Fatalf("bind of a child of a session bound before = %d %v", s, out)
		}
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		e.plugin.SetParent("s-mine-child", sessionMine)
		for _, sid := range []string{sessionMine, "s-mine-child"} {
			if s, out := bind(sid); s != http.StatusOK || out[keyDeviceID] != id {
				t.Fatalf("bind of %s = %d %v", sid, s, out)
			}
		}
		a := e.acpLeg(tok)
		res, _ := a.request(acpNew, map[string]any{keyCWD: deviceDir})[keyResult].(map[string]any)
		created, _ := res[keySessionACP].(string)
		e.plugin.SetParent(created, sessionGone)
		if s, out := bind(created); s != http.StatusOK || out[keyDeviceID] != id {
			t.Fatalf("bind of a session the device created = %d %v", s, out)
		}
		g := e.gatewayLeg(e.a, tok)
		cres, _ := g.request("c1", methodCreate, map[string]any{keyRequestID: "q1", keyCWD: deviceDir})[keyResult].(map[string]any)
		sid, _ := cres[keySessionID].(string)
		if s, out := call(t, http.MethodPut, e.b.url+"/api/device/sessions/"+sid+"/binding", tok, map[string]any{keyDir: otherDir}); s != http.StatusOK ||
			out[keyDir] != otherDir {
			t.Fatalf("resume bind of the device's own session = %d %v", s, out)
		}
	})
}

// TestGatewayLegCreateParams covers session.create params that reach
// beyond the new session: title (returns an existing session), base_url
// and api_key (send the profile's key to the device's host),
// accept_hooks (profile-wide hook consent) and cwd_explicit false (the
// profile's workspace in the pod).
func TestGatewayLegCreateParams(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		e.connectExec(e.a, tok)
		g := e.gatewayLeg(e.b, tok)
		for i, extra := range []map[string]any{
			{keyTitle: sessionOther}, {keyTitle: "My chat"}, {keyTitle: nil},
			{"base_url": "https://attacker.example.com/v1"}, {"base_url": nil}, {"api_key": "sk-x"},
			{"accept_hooks": true}, {"accept_hooks": false},
		} {
			p := map[string]any{keyRequestID: "q", keyCWD: deviceDir, keySource: sourceTUI}
			for k, v := range extra {
				p[k] = v
			}
			if m := g.request(i, methodCreate, p); reason(m) != reasonDenied {
				t.Fatalf("create %v = %v", extra, m)
			}
		}
		if got := e.called(methodCreate); len(got) != 0 {
			t.Fatalf("refused creates reached the gateway: %v", got)
		}
		if m := g.request("ok", methodCreate, map[string]any{keyRequestID: "q2", keyCWD: deviceDir, "cwd_explicit": false}); m[keyResult] == nil {
			t.Fatalf("create = %v", m)
		}
		if got := e.called(methodCreate); len(got) != 1 || got[0].Params["cwd_explicit"] != nil {
			t.Fatalf("create reached the gateway as %v", got)
		}
	})
}

// TestGatewayLegCreateExisting covers a create answered with a session
// that already exists (a request_id another device used): the hub must
// neither bind it nor delete it.
func TestGatewayLegCreateExisting(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tokA, idA := e.pair(e.a)
		tokB, _ := e.pairAs(e.a, "laptop")
		e.connectExec(e.a, tokA)
		e.connectExec(e.a, tokB)
		a := e.gatewayLeg(e.a, tokA)
		res, _ := a.request("a1", methodCreate, map[string]any{keyRequestID: "shared", keyCWD: deviceDir})[keyResult].(map[string]any)
		sid, _ := res[keySessionID].(string)
		if e.plugin.Bindings()[sid].DeviceID != idA {
			t.Fatalf("binding = %+v", e.plugin.Bindings()[sid])
		}
		b := e.gatewayLeg(e.a, tokB)
		if m := b.request("b1", methodCreate, map[string]any{keyRequestID: "shared", keyCWD: otherDir}); reason(m) != reasonOtherDevice {
			t.Fatalf("create of another device's session = %v", m)
		}
		if got := e.plugin.Bindings()[sid]; got.DeviceID != idA || got.Dir != deviceDir {
			t.Fatalf("binding after the other device's create = %+v", got)
		}
		res, _ = a.request("a2", methodCreate, map[string]any{keyRequestID: "was-bound", keyCWD: deviceDir})[keyResult].(map[string]any)
		gone, _ := res[keySessionID].(string)
		if s, _ := call(t, http.MethodDelete, e.a.url+"/api/device/sessions/"+gone+"/binding", tokA, nil); s != http.StatusNoContent {
			t.Fatalf("unbind = %d", s)
		}
		if m := b.request("b2", methodCreate, map[string]any{keyRequestID: "was-bound", keyCWD: otherDir}); reason(m) != "hermes_device_bind" {
			t.Fatalf("create of a session bound before = %v", m)
		}
		if got := e.called(methodMutate); len(got) != 0 {
			t.Fatalf("the hub deleted an existing session: %v", got)
		}
		if !slices.Contains(e.gw.SessionIDs(), sid) || !slices.Contains(e.gw.SessionIDs(), gone) {
			t.Fatalf("sessions = %v", e.gw.SessionIDs())
		}
	})
}

// TestGatewayLegParams covers the smaller param rules: delegation.status
// and spawn trees (other sessions), config.get with cwd (git in a pod
// directory), render sizes, model.options refresh, non-string profiles and
// request_ids in the hub's own form.
func TestGatewayLegParams(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.plugin.SetBinding("s1", id, deviceDir)
		g := e.gatewayLeg(e.b, tok)
		for _, tc := range []struct {
			method string
			params map[string]any
		}{
			{"delegation.status", map[string]any{}},
			{methodSpawnList, map[string]any{keySessionID: "s1", "cross_session": true}},
			{methodSpawnList, map[string]any{keySessionID: "s1"}},
			{"spawn_tree.load", map[string]any{keyPath: "/opt/data/spawn-trees/s-other/x.json"}},
			{methodConfigGet, map[string]any{keyKey: keyProject, keyCWD: podDir}},
			{methodConfigGet, map[string]any{keySessionID: "s1", keyKey: keyProject, keyCWD: nil}},
			{methodFileAttach, map[string]any{keySessionID: "s1", "staged_upload": map[string]any{"profile_home": podDir, keyPath: "uploads/x"}}},
			{methodFileAttach, map[string]any{keySessionID: "s1", keyPath: podDir + "/.env"}},
			{methodList, map[string]any{keyProfile: 1}},
			{methodList, map[string]any{keyProfile: []any{nameOther}}},
			{methodList, map[string]any{keyProfile: ""}},
			{methodList, map[string]any{keyProfile: map[string]any{"name": nameOther}}},
		} {
			if m := g.request("r-"+tc.method, tc.method, tc.params); reason(m) != reasonDenied {
				t.Fatalf("%s %v = %v", tc.method, tc.params, m)
			}
		}
		if got := len(e.called(methodConfigGet)) + len(e.called(methodList)); got != 0 {
			t.Fatalf("%d refused calls reached the gateway", got)
		}
		if m := g.request("own", methodMutate, map[string]any{keySessionID: "s1", keyRequestID: "hermes-hub-delete-s1",
			keyOperation: "rename", keyPayload: map[string]any{keyTitle: "x"}}); errCode(m) != codeInvalid {
			t.Fatalf("own request_id = %v", m)
		}
		if m := g.request("cfg", methodConfigGet, map[string]any{keySessionID: "s1", keyKey: keyProject}); reason(m) == reasonDenied {
			t.Fatalf("config.get = %v", m)
		}
		g.request("lf", "learning.frames", map[string]any{keyCols: 1e9, "rows": -5, "frames": 1e300})
		g.request("pc", "pet.cells", map[string]any{keyCols: 99999999, "state": "idle"})
		g.request("pc0", "pet.cells", map[string]any{keyCols: nil})
		g.request("mo", "model.options", map[string]any{keySessionID: "s1", "refresh": true, "explicit_only": true})
		for _, tc := range []struct {
			method string
			want   map[string]any
		}{
			{"learning.frames", map[string]any{keyCols: 400.0, "rows": 10.0, "frames": 120.0}},
			{"model.options", map[string]any{keySessionID: "s1", "explicit_only": true}},
		} {
			got := e.called(tc.method)
			if len(got) != 1 || len(got[0].Params) != len(tc.want) {
				t.Fatalf("%s reached the gateway as %v", tc.method, got)
			}
			for k, v := range tc.want {
				if got[0].Params[k] != v {
					t.Fatalf("%s reached the gateway as %v", tc.method, got)
				}
			}
		}
		if got := e.called("pet.cells"); len(got) != 2 || got[0].Params[keyCols] != 200.0 || got[1].Params[keyCols] != nil {
			t.Fatalf("pet.cells reached the gateway as %v", got)
		}
		if m := g.request("bad", "pet.cells", map[string]any{keyCols: "9999"}); errCode(m) != codeBadParams {
			t.Fatalf("string cols = %v", m)
		}
	})
}

// TestGatewayLegAnswers covers device messages with no method: only an
// answer to a gateway request (string srq- id, exactly one of result and
// error) goes through, rebuilt from those members.
func TestGatewayLegAnswers(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		g := e.gatewayLeg(e.b, tok)
		for _, m := range []map[string]any{
			{"id": "r1", keyResult: map[string]any{}},
			{"id": 5, keyResult: map[string]any{}},
			{"id": "srq-2", keyResult: map[string]any{}, "error": map[string]any{"code": 1}},
			{"id": "srq-3"},
			{keyResult: map[string]any{}},
			{"id": "srq-4", keyMethod: "", keyResult: map[string]any{}},
		} {
			m[keyJSONRPC] = jsonRPC
			writeJSON(t, g.c, m)
		}
		writeJSON(t, g.c, map[string]any{keyJSONRPC: "1.0", "id": "srq-1", keyResult: map[string]any{keyValue: "yes"},
			keyParams: map[string]any{keySessionID: sessionOther}, keySessionID: sessionOther, "adopt": true})
		if m := readJSON(t, g.c); errCode(m) != -32601 || m["id"] != "srq-4" {
			t.Fatalf("empty method = %v", m)
		}
		g.request("fence", methodList, map[string]any{})
		var got []json.RawMessage
		for _, c := range e.gw.Calls() {
			if c.Method == "" {
				got = append(got, c.Raw)
			}
		}
		if len(got) != 1 || string(got[0]) != `{"id":"srq-1","jsonrpc":"2.0","result":{"value":"yes"}}` {
			t.Fatalf("gateway got %s", got)
		}
	})
}

// TestGatewayLegPromptSlash covers slash commands typed as prompt text:
// Hermes dispatches a prompt.submit text that starts with / as a command
// (gateway/platforms/event.py get_command), so the hub applies the slash
// allowlist to it in every form Hermes reads.
func TestGatewayLegPromptSlash(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.plugin.SetBinding("s1", id, deviceDir)
		g := e.gatewayLeg(e.b, tok)
		refused := []string{
			"/reload-mcp", "  /reload-mcp", "\t\n/reload-mcp", "\x1c/reload-mcp", " /reload-mcp",
			"/reload_mcp@hermes_bot", "/RELOAD-MCP", "/Reload-Mcp now", "@file:/tmp/a.txt\n/reload-mcp",
			"@image:x\n@url:https://example.com\n  /reload-mcp", "@ımage:x\n/reload-mcp", "@FILE:x\n/sethome",
			"/skills install x", "/my-quick-exec", "/kanban run", "/cron add", "/verbose", "/diff", "/refine", "/personality pirate",
			"/fast on --global", "/fast on --GLOBAL", "/fast on —global", "/fast on –-global", "/fast on \"--glo\"bal",
			"/fast on '-'-global", "/yolo ‒global", "/reasoning show", "/reasoning high --global", "/reasoning high x",
			"/model gpt-5", "/model gpt-5 --global", "/model gpt-5 --session --refresh", "/model@bot gpt-5", "/vıew",
		}
		allowed := []string{
			"hello", "/etc/passwd is odd", "/ hi", "/", "@file:/tmp/a.txt\nsummarize it", "/title A new name", "/fast on",
			"/compact", "/q next question", "/RESET", "/reasoning high", "/model gpt-5 --session", "/model gpt-5 —session",
			"/steer@hermes_bot go left", "/btw what is this",
		}
		for _, method := range []string{"prompt.submit", "prompt.replace_queued", "session.steer", "session.redirect", "prompt.btw"} {
			for i, text := range refused {
				p := map[string]any{keySessionID: "s1", keyText: text, keySubmission: "x"}
				if m := g.request(fmt.Sprintf("%s-r%d", method, i), method, p); reason(m) != reasonDenied {
					t.Fatalf("%s %q = %v", method, text, m)
				}
			}
			if got := e.called(method); len(got) != 0 {
				t.Fatalf("refused %s calls reached the gateway: %v", method, got)
			}
			for i, text := range allowed {
				p := map[string]any{keySessionID: "s1", keyText: text, keySubmission: "x"}
				if m := g.request(fmt.Sprintf("%s-a%d", method, i), method, p); reason(m) == reasonDenied {
					t.Fatalf("%s %q = %v", method, text, m)
				}
			}
			if got := e.called(method); len(got) != len(allowed) {
				t.Fatalf("%s calls = %d, want %d", method, len(got), len(allowed))
			}
		}
		for _, cmd := range []string{"/verbose", "/diff", "/refine", "/fast on —global", "/fast on --GLOBAL", "/model x --refresh"} {
			if m := g.request("sx-"+cmd, methodSlash, map[string]any{keySessionID: "s1", keyCommand: cmd}); reason(m) != reasonDenied {
				t.Fatalf("slash.exec %q = %v", cmd, m)
			}
		}
		g.request("sx-model", methodSlash, map[string]any{keySessionID: "s1", keyCommand: "/model gpt-5"})
		g.request("cd-model", "command.dispatch", map[string]any{keySessionID: "s1", keyName: "model", "arg": "gpt-5"})
		if got := e.called(methodSlash); len(got) != 1 || got[0].Params[keyCommand] != "/model gpt-5 --session" {
			t.Fatalf("slash.exec /model reached the gateway as %v", got)
		}
		if got := e.called("command.dispatch"); len(got) != 1 || got[0].Params["arg"] != "gpt-5 --session" {
			t.Fatalf("command.dispatch model reached the gateway as %v", got)
		}
	})
}

// TestACPLegPromptSlash covers slash commands in ACP prompt text: hermes acp
// sends a prompt that names a registered command to the gateway as
// slash.exec over its own connection, past the gateway leg's checks.
func TestACPLegPromptSlash(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		a := e.acpLeg(tok)
		n := len(e.acp.Received())
		text := func(s string) map[string]any { return map[string]any{keyType: keyText, keyText: s} }
		for _, prompt := range [][]any{
			{text("/approvals off")}, {text("  /verbose")}, {text("/diff")}, {text("/personality pirate")}, {text("/memory approve")},
			{text("/skills approve x")}, {text("/my-quick-exec")}, {text(""), text("/approvals off")}, {text("hi"), text("/approvals off")},
			{map[string]any{keyType: "resource", "resource": map[string]any{"uri": "file:///a", keyText: "/approvals off"}}},
			{text("/model gpt-5 --reasoning --session --reasoning high")}, {text("/usage reset --force")},
		} {
			if m := a.request(acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: prompt}); errCode(m) != codeBadParams ||
				reason(m) != reasonDenied {
				t.Fatalf("prompt %v = %v", prompt, m)
			}
		}
		if got := e.acp.Received()[n:]; len(got) != 0 {
			t.Fatalf("refused prompts reached the agent: %v", got)
		}
		for _, prompt := range [][]any{{text("hi")}, {text("/etc/hosts is wrong")}, {text("/title x")}, {text("/compact")}} {
			if m := a.request(acpPrompt, map[string]any{keySessionACP: sessionMine, keyPrompt: prompt}); m[keyResult] == nil {
				t.Fatalf("prompt %v = %v", prompt, m)
			}
		}
	})
}
