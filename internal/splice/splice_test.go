package splice_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/coder/websocket"
)

func TestCreateRewriteHoldsUntilBind(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	gate := make(chan struct{})
	e.hub.mu.Lock()
	e.hub.bindGate = gate
	e.hub.mu.Unlock()
	c := e.dialTUI()
	c.request("r1", "session.create", createParams("req-1", deviceDir))

	waitFor(t, "the create at the gateway", func() bool { _, ok := e.hub.find("session.create", nil); return ok })
	got, _ := e.hub.find("session.create", nil)
	if got.Params[keyCwd] != placeholder || got.Params["row_cwd"] != deviceDir || got.Params[keyRequestID] != "req-1" ||
		got.Params["source"] != sourceTUI || got.Params["cwd_explicit"] != true || got.Params["client_env"] == nil {
		t.Fatalf("gateway got %+v", got.Params)
	}
	// Other traffic flows while the create is held.
	c.call("r2", "test.echo", map[string]any{})
	if m, err := c.readTimeout(200 * time.Millisecond); !errors.Is(err, errNoFrame) {
		t.Fatalf("the TUI got %+v (%v) before the bind finished", m, err)
	}
	close(gate)
	m := c.read()
	if string(m.ID) != `"r1"` || resultMap(t, m)[keySessionID] != sessionReq1 {
		t.Fatalf("answer %+v", m)
	}
	if b := e.hub.bindCalls(); len(b) != 1 || b[0] != (bindCall{sessionReq1, deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
}

func TestCreateBindFailureDeletes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.bindFail[deviceDir] = hubOffline
	c := e.dialTUI()
	m := c.call("r1", "session.create", createParams("req-1", deviceDir))
	if m.Error == nil || m.Error.Code != splice.CodeDevice || m.Error.Data[keyReason] != splice.ReasonBind ||
		m.Error.Data["hub_error"] != hubOffline {
		t.Fatalf("answer %+v", m)
	}
	del, ok := e.hub.find(methodMutate, func(p map[string]any) bool { return p[keyOperation] == "delete" })
	if !ok {
		t.Fatal("no delete mutate")
	}
	var id string
	_ = json.Unmarshal(del.ID, &id)
	p := del.Params
	if p[keySessionID] != sessionReq1 || p[keyRequestID] != "hermes-device-delete-s-req-1" ||
		p["expected_revision"] != float64(1) || p["expected_generation"] != float64(0) ||
		len(asMap(t, p[keyPayload])) != 0 || len(id) < len(splice.OwnIDPrefix) || id[:len(splice.OwnIDPrefix)] != splice.OwnIDPrefix {
		t.Fatalf("delete %s %+v", del.ID, p)
	}
	// The splice consumed the delete's answer: the next frame is the echo.
	if res := resultMap(t, c.call("r2", "test.echo", map[string]any{})); res[keyMethod] != "test.echo" {
		t.Fatalf("next answer %+v", res)
	}
}

func TestCreateRetryBindsOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	a := resultMap(t, c.call("r1", "session.create", createParams("req-1", deviceDir)))
	b := resultMap(t, c.call("r2", "session.create", createParams("req-1", deviceDir)))
	if a[keySessionID] != b[keySessionID] {
		t.Fatalf("sessions %v and %v", a[keySessionID], b[keySessionID])
	}
	if n := len(e.hub.bindCalls()); n != 1 {
		t.Fatalf("%d binds, want 1", n)
	}
	e.hub.mu.Lock()
	calls := e.hub.whoamiCalls
	e.hub.mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d whoami calls, want 1 (cached)", calls)
	}
}

func TestCreateCwdChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	for i, cwd := range []string{"", "src/app"} {
		m := c.call(i, "session.create", createParams("req", cwd))
		if m.Error == nil || m.Error.Code != splice.CodeDevice || m.Error.Data[keyReason] != splice.ReasonCwd {
			t.Fatalf("cwd %q: answer %+v", cwd, m)
		}
	}
	if _, ok := e.hub.find("session.create", nil); ok {
		t.Fatal("a refused create reached the gateway")
	}
}

func TestWhoamiFailureThenRecovery(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.whoamiFail = true
	c := e.dialTUI()
	m := c.call("r1", "session.create", createParams("req-1", deviceDir))
	if m.Error == nil || m.Error.Data[keyReason] != splice.ReasonBind || m.Error.Data["hub_error"] != hubBackendDown {
		t.Fatalf("answer %+v", m)
	}
	e.hub.mu.Lock()
	e.hub.whoamiFail = false
	e.hub.mu.Unlock()
	if res := resultMap(t, c.call("r2", "session.create", createParams("req-1", deviceDir))); res[keyCwd] != placeholder {
		t.Fatalf("result %+v", res)
	}
}

func TestWorkspaceRebind(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.create", createParams("req-1", deviceDir)))
	other := "/home/alice/src/other"
	resultMap(t, c.call("r2", methodMutate, map[string]any{keySessionID: sessionReq1, keyRequestID: "m1",
		"expected_revision": 1, "expected_generation": 0, keyOperation: opWorkspace, keyPayload: map[string]any{keyCwd: other}}))
	binds := e.hub.bindCalls()
	if len(binds) != 2 || binds[1] != (bindCall{sessionReq1, other}) {
		t.Fatalf("binds %+v", binds)
	}
	mut, _ := e.hub.find(methodMutate, nil)
	if asMap(t, mut.Params[keyPayload])[keyCwd] != placeholder || mut.Params[keyRequestID] != "m1" {
		t.Fatalf("gateway got %+v", mut.Params)
	}

	e.hub.mu.Lock()
	e.hub.bindFail["/home/alice/denied"] = "dir_refused"
	e.hub.mu.Unlock()
	m := c.call("r3", methodMutate, map[string]any{keySessionID: sessionReq1, keyRequestID: "m2",
		keyOperation: opWorkspace, keyPayload: map[string]any{keyCwd: "/home/alice/denied"}})
	if m.Error == nil || m.Error.Data["hub_error"] != "dir_refused" {
		t.Fatalf("answer %+v", m)
	}
	if _, ok := e.hub.find(methodMutate, func(p map[string]any) bool { return p[keyRequestID] == "m2" }); ok {
		t.Fatal("a workspace move whose bind failed reached the gateway")
	}
	m = c.call("r4", methodMutate, map[string]any{keySessionID: sessionReq1, keyOperation: opWorkspace,
		keyPayload: map[string]any{keyCwd: "relative"}})
	if m.Error == nil || m.Error.Data[keyReason] != splice.ReasonCwd {
		t.Fatalf("answer %+v", m)
	}
}

func TestWorkspaceRefusedRestoresBinding(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.create", createParams("busy", deviceDir)))
	m := c.call("r2", methodMutate, map[string]any{keySessionID: busySession, keyOperation: opWorkspace,
		keyPayload: map[string]any{keyCwd: "/home/alice/b"}})
	if m.Error == nil {
		t.Fatalf("answer %+v", m)
	}
	waitFor(t, "the restoring bind", func() bool { return len(e.hub.bindCalls()) == 3 })
	if b := e.hub.bindCalls(); b[1] != (bindCall{busySession, "/home/alice/b"}) || b[2] != (bindCall{busySession, deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
}

func TestBranchBindsChild(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.create", createParams("req-1", deviceDir)))
	res := resultMap(t, c.call("r2", methodMutate, map[string]any{keySessionID: sessionReq1, keyRequestID: "b1",
		"expected_revision": 1, "expected_generation": 0, keyOperation: opBranch, keyPayload: map[string]any{}}))
	if res["branched_session_id"] != childReq1 {
		t.Fatalf("result %+v", res)
	}
	binds := e.hub.bindCalls()
	if len(binds) != 2 || binds[1] != (bindCall{childReq1, deviceDir}) {
		t.Fatalf("binds %+v", binds)
	}
	// A branch of a session this daemon never bound passes without a bind.
	resultMap(t, c.call("r3", methodMutate, map[string]any{keySessionID: "other", keyOperation: opBranch, keyPayload: map[string]any{}}))
	if n := len(e.hub.bindCalls()); n != 2 {
		t.Fatalf("%d binds, want 2", n)
	}
	// A failed child bind still passes the answer.
	e.hub.mu.Lock()
	e.hub.bindFail[deviceDir] = hubOffline
	e.hub.mu.Unlock()
	if res := resultMap(t, c.call("r4", methodMutate, map[string]any{keySessionID: sessionReq1, keyOperation: opBranch,
		keyPayload: map[string]any{}})); res["branched_session_id"] != childReq1 {
		t.Fatalf("result %+v", res)
	}
}

// deniedConfig reports whether a config.set the gateway got is one the
// test expects the splice to refuse.
func deniedConfig(p map[string]any) bool {
	switch p[keyConfigKey] {
	case "verbose", "yolo":
		return false
	case keyReasoning:
		return p[keyConfigValue] == "show" || p[keyScope] == scopeGlobal
	}
	return true
}

func TestDeniedMethods(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	cases := []struct {
		method string
		params map[string]any
	}{
		{methodConfigSet, map[string]any{keyConfigKey: keyCwd, keyConfigValue: "/x"}},
		{methodConfigSet, map[string]any{keyConfigKey: "terminal.cwd", keyConfigValue: "/x"}},
		{methodConfigSet, map[string]any{}},
		{methodConfigSet, map[string]any{keyConfigKey: "fast", keyConfigValue: "on", keyScope: scopeGlobal}},
		{methodConfigSet, map[string]any{keyConfigKey: keyReasoning, keyConfigValue: "show"}},
		{methodConfigSet, map[string]any{keyConfigKey: keyReasoning, keyConfigValue: "high", keyScope: scopeGlobal}},
		{"vault.list", nil},
		{"vault.set", map[string]any{keyName: "k"}},
		{"model.save_key", map[string]any{}},
		{"profiles.create", map[string]any{}},
		{"shell.exec", map[string]any{keyCommand: "id"}},
		{"cli.exec", map[string]any{}},
		{"image.attach", map[string]any{keyPath: "/etc/passwd"}},
		{methodPDFAttach, map[string]any{keyPath: "/x.pdf"}},
		{methodFileAttach, map[string]any{keyPath: "/x"}},
		{"input.detect_drop", map[string]any{}},
		{"clipboard.paste", map[string]any{}},
		{"mcp.servers.add", map[string]any{}},
		{"mcp.servers.test", map[string]any{}},
		{"plugins.manage", map[string]any{}},
		{"skills.manage", map[string]any{}},
		{"cron.manage", map[string]any{}},
		{"browser.manage", map[string]any{}},
		{"prompt.background", map[string]any{}},
		{methodMutate, map[string]any{keySessionID: "s", keyOperation: "editor_mcp", keyPayload: map[string]any{}}},
		{"mcp.servers.remove", map[string]any{}},
		{"cron.submit", map[string]any{}},
		{"kanban.run", map[string]any{}},
		{"display.install", map[string]any{}},
		{"display.start", map[string]any{}},
		{"a2a.forward", map[string]any{}},
		{"worker.spawn", map[string]any{}},
		{"process.kill", map[string]any{}},
		{"process.list", map[string]any{}},
		{"rollback.restore", map[string]any{}},
		{"connectors.add", map[string]any{}},
		{"set_api_key", map[string]any{}},
		{"oauth.start", map[string]any{}},
		{"diagnostics.share_nous", map[string]any{}},
		{"free_tier.provision", map[string]any{}},
		{"session.foreign.import", map[string]any{}},
	}
	for i, tc := range cases {
		m := c.call(i, tc.method, tc.params)
		if m.Error == nil || m.Error.Code != splice.CodeDenied || m.Error.Data[keyReason] != splice.ReasonDenied ||
			m.Error.Message != tc.method+" is not available through hermes-device" {
			t.Errorf("%s %v: answer %+v", tc.method, tc.params, m)
		}
	}
	allowed := []struct {
		method string
		params map[string]any
	}{
		{methodConfigSet, map[string]any{keyConfigKey: "verbose", keyConfigValue: "all", keySessionID: "s"}},
		{methodConfigSet, map[string]any{keyConfigKey: "yolo", keyConfigValue: "on", keySessionID: "s"}},
		{methodConfigSet, map[string]any{keyConfigKey: keyReasoning, keyConfigValue: "high", keyScope: "session"}},
		{methodConfigSet, map[string]any{keyConfigKey: keyReasoning, keyConfigValue: "reset"}},
		{methodPDFAttach, map[string]any{"content_base64": "AA=="}},
		{methodFileAttach, map[string]any{"data_url": "data:,", keyPath: nil}},
		{"image.attach_bytes", map[string]any{keyData: "AA=="}},
		{"slash.exec", map[string]any{keyCommand: "/help"}},
		{"command.dispatch", map[string]any{keyName: "help"}},
	}
	for i, tc := range allowed {
		if res := resultMap(t, c.call(100+i, tc.method, tc.params)); res[keyMethod] != tc.method {
			t.Errorf("%s: result %+v", tc.method, res)
		}
	}
	for _, m := range e.hub.receivedMethods() {
		for _, tc := range cases {
			if m.Method == tc.method && m.Method != methodMutate && (m.Method != methodConfigSet || deniedConfig(m.Params)) &&
				(m.Method != methodPDFAttach && m.Method != methodFileAttach || m.Params[keyPath] != nil) {
				t.Errorf("denied %s reached the gateway: %+v", m.Method, m.Params)
			}
		}
	}
}

func TestCompletePathEmpty(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	m := c.call("r1", "complete.path", map[string]any{"word": "/etc/"})
	if m.Error != nil || string(m.Result) != `{"items":[]}` {
		t.Fatalf("answer %+v %s", m, m.Result)
	}
	if _, ok := e.hub.find("complete.path", nil); ok {
		t.Fatal("complete.path reached the gateway")
	}
}

func TestStringAndIntegerIDs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	for _, id := range []any{"r7", 7, "heartbeat-1"} {
		c.call(id, "test.echo", map[string]any{})
	}
	res := resultMap(t, c.call(42, "session.create", createParams("int-id", deviceDir)))
	if res[keySessionID] != "s-int-id" {
		t.Fatalf("result %+v", res)
	}
	if m := c.call(43, "shell.exec", map[string]any{}); m.Error == nil {
		t.Fatal("integer-id denial missing")
	}
	// The gateway saw the TUI's ids as sent.
	got, _ := e.hub.find("session.create", nil)
	if string(got.ID) != "42" {
		t.Fatalf("gateway id %s", got.ID)
	}
}

func TestServerRequestRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	c.request("r1", "test.server_request", map[string]any{})
	req := c.read()
	if req.Method != "approval.request" || string(req.ID) != `"srv-1"` {
		t.Fatalf("server request %+v", req)
	}
	c.send(map[string]any{keyJSONRPC: jsonRPC, "id": "srv-1", "result": map[string]any{"choice": "once"}})
	m := c.read()
	if string(m.ID) != `"r1"` || string(m.Result) != `{"answer":{"choice":"once"}}` {
		t.Fatalf("answer %+v %s", m, m.Result)
	}
}

func TestUpgradeChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	token := e.newToken()
	cases := []struct {
		name   string
		path   string
		header http.Header
	}{
		{"wrong token", splice.PathTUI + "?token=" + token + "x", nil},
		{"no token", splice.PathTUI, nil},
		{"origin", splice.PathTUI + "?token=" + token, http.Header{"Origin": {"http://127.0.0.1"}}},
		{"empty origin", splice.PathTUI + "?token=" + token, http.Header{"Origin": {""}}},
		{"wrong path", "/api/wsx?token=" + token, nil},
		{"other path", "/ws?token=" + token, nil},
	}
	for _, tc := range cases {
		_, resp, err := websocket.Dial(t.Context(), "ws://"+e.addr+tc.path, &websocket.DialOptions{HTTPHeader: tc.header})
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Errorf("%s: err %v status %d, want 403", tc.name, err, code)
		}
	}
	ws, _, err := websocket.Dial(t.Context(), "ws://"+e.addr+splice.PathTUI+"?token="+token, nil)
	if err != nil {
		t.Fatalf("good token: %v", err)
	}
	_ = ws.CloseNow()
}

func TestControlEndpoints(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	if r := e.control(http.MethodPost, splice.PathControlTokens, "wrong"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong secret: %d", r.StatusCode)
	}
	if r := e.control(http.MethodPost, splice.PathControlTokens, ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no secret: %d", r.StatusCode)
	}
	for _, body := range []string{"", "{}", `{"cwd": "rel"}`, "[]"} {
		if r := e.controlBody(http.MethodPost, splice.PathControlTokens, e.splice.ControlSecret(), body); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: %d", body, r.StatusCode)
		}
	}
	if len(e.splice.ControlSecret()) != 43 {
		t.Fatalf("control secret %q", e.splice.ControlSecret())
	}
	other := e.dialTUI()
	token := e.newToken()
	c := e.dialTUIToken(token)
	c2 := e.dialTUIToken(token)
	if r := e.control(http.MethodDelete, splice.PathControlTokens+"/"+token, "wrong"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("delete with wrong secret: %d", r.StatusCode)
	}
	if r := e.control(http.MethodDelete, splice.PathControlTokens+"/"+token, e.splice.ControlSecret()); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	for _, s := range []*tui{c, c2} {
		if _, err := s.readTimeout(10 * time.Second); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Fatalf("revoked socket read: %v", err)
		}
	}
	_, resp, err := websocket.Dial(t.Context(), "ws://"+e.addr+splice.PathTUI+"?token="+token, nil)
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked token dial: %v", err)
	}
	// Sockets of other tokens stay up.
	other.call("r1", "test.echo", map[string]any{})
}

func TestHubLegDropClosesWithoutAnswers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	c.request("b1", methodMutate, map[string]any{keySessionID: holdID, keyOperation: opBranch, keyPayload: map[string]any{}})
	c.request("c1", "session.create", createParams(holdID, deviceDir))
	waitFor(t, "both held at the gateway", func() bool {
		_, a := e.hub.find(methodMutate, nil)
		_, b := e.hub.find("session.create", nil)
		return a && b
	})
	e.hub.dropLegs()
	// Held requests get no answer, so the TUI keeps the create's
	// request_id for its retry; the socket closes with 1011 instead.
	m, err := c.readTimeout(10 * time.Second)
	if err == nil {
		t.Fatalf("held request answered after the drop: %+v", m)
	}
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("after the drop: %v", err)
	}
}

func TestCreateBypassRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	for i, field := range []string{"safe_mode", "ignore_user_config"} {
		p := createParams("bypass", deviceDir)
		p[field] = true
		m := c.call(i, "session.create", p)
		if m.Error == nil || m.Error.Code != splice.CodeDevice || m.Error.Data[keyReason] != splice.ReasonBypass {
			t.Fatalf("%s: answer %+v", field, m)
		}
	}
	if _, ok := e.hub.find("session.create", nil); ok {
		t.Fatal("a bypass create reached the gateway")
	}
	p := createParams("plain", deviceDir)
	p["safe_mode"] = false
	resultMap(t, c.call(9, "session.create", p))
}

func TestEditorMCPServersRemoved(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	p := createParams("req-1", deviceDir)
	p["editor"] = map[string]any{keyName: "zed", keyMCPServers: []any{map[string]any{keyCommand: "x"}}}
	resultMap(t, c.call("r1", "session.create", p))
	got, _ := e.hub.find("session.create", nil)
	if ed := asMap(t, got.Params["editor"]); ed[keyName] != "zed" || ed[keyMCPServers] != nil {
		t.Fatalf("editor at the gateway %+v", ed)
	}
	resultMap(t, c.call("r2", methodMutate, map[string]any{keySessionID: sessionReq1, keyOperation: opBranch,
		keyPayload: map[string]any{keyTitle: "t", keyMCPServers: []any{}}}))
	br, _ := e.hub.find(methodMutate, nil)
	if pl := asMap(t, br.Params[keyPayload]); pl[keyTitle] != "t" || pl[keyMCPServers] != nil {
		t.Fatalf("branch payload at the gateway %+v", pl)
	}
	resultMap(t, c.call("r3", "session.resume", map[string]any{keySessionID: sessionReq1,
		"editor": map[string]any{keyMCPServers: []any{}}}))
	rs, _ := e.hub.find("session.resume", nil)
	if ed := asMap(t, rs.Params["editor"]); len(ed) != 0 {
		t.Fatalf("resume editor at the gateway %+v", ed)
	}
}

func TestResumeBindsToChatDir(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.resume", map[string]any{keySessionID: oldSession}))
	if b := e.hub.bindCalls(); len(b) != 1 || b[0] != (bindCall{oldSession, deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
	// A move, then a resume on the same chat (a reconnect), keeps the move.
	other := "/home/alice/src/other"
	resultMap(t, c.call("r2", methodMutate, map[string]any{keySessionID: oldSession, keyOperation: opWorkspace,
		keyPayload: map[string]any{keyCwd: other}}))
	resultMap(t, c.call("r3", "session.resume", map[string]any{keySessionID: oldSession}))
	if b := e.hub.bindCalls(); len(b) != 2 || b[1] != (bindCall{oldSession, other}) {
		t.Fatalf("binds %+v", b)
	}
	// A failed bind refuses the resume.
	e.hub.mu.Lock()
	e.hub.bindFail[deviceDir] = "bound_to_other_device"
	e.hub.mu.Unlock()
	m := c.call("r5", "session.resume", map[string]any{keySessionID: "theirs"})
	if m.Error == nil || m.Error.Data["hub_error"] != "bound_to_other_device" {
		t.Fatalf("answer %+v", m)
	}
	if _, ok := e.hub.find("session.resume", func(p map[string]any) bool { return p[keySessionID] == "theirs" }); ok {
		t.Fatal("a resume whose bind failed reached the gateway")
	}
}

// resumeIDs returns the session_id of every session.resume the gateway got.
func (h *fakeHub) resumeIDs() []any {
	var ids []any
	for _, m := range h.receivedMethods() {
		if m.Method == "session.resume" {
			ids = append(ids, m.Params[keySessionID])
		}
	}
	return ids
}

func TestResumeLatestResolvesToBoundSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.mu.Lock()
	e.hub.stored = []string{webChat, "other-dev", latestSession}
	e.hub.live = []string{liveSession}
	e.hub.notMine[webChat] = reasonNotBound
	e.hub.notMine["other-dev"] = "hermes_device_other_device"
	e.hub.mu.Unlock()
	c := e.dialTUI()
	for i, params := range []map[string]any{
		{keyLatest: sourceTUI, "workspace": "/home/alice/src"},
		{keySessionID: ""},
		{},
	} {
		res := resultMap(t, c.call(fmt.Sprintf("r%d", i), "session.resume", params))
		if res[keySessionID] != latestSession {
			t.Fatalf("resume %v: result %+v", params, res)
		}
	}
	got, ok := e.hub.find("session.resume", nil)
	if !ok {
		t.Fatal("no resume reached the gateway")
	}
	if _, has := got.Params[keyLatest]; has || got.Params["workspace"] != nil {
		t.Fatalf("resume params %+v", got.Params)
	}
	// One bind: the later resumes are of a session this chat bound.
	if b := e.hub.bindCalls(); len(b) != 1 || b[0] != (bindCall{latestSession, deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
	if ids := e.hub.resumeIDs(); len(ids) != 3 {
		t.Fatalf("resumes %v", ids)
	}

	// No session of this device: the TUI gets not_found and nothing is
	// resumed or bound.
	e.hub.mu.Lock()
	e.hub.stored = []string{webChat}
	e.hub.live = []string{"other-dev"}
	e.hub.mu.Unlock()
	m := c.call("r9", "session.resume", map[string]any{keyLatest: sourceTUI})
	if m.Error == nil || m.Error.Data[keyReason] != splice.ReasonNotFound {
		t.Fatalf("answer %+v", m)
	}
	if ids := e.hub.resumeIDs(); len(ids) != 3 {
		t.Fatalf("resumes %v", ids)
	}
}

func TestResumeLatestFindsLiveSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.mu.Lock()
	e.hub.stored = []string{webChat}
	e.hub.live = []string{webChat, liveSession}
	e.hub.notMine[webChat] = reasonNotBound
	e.hub.mu.Unlock()
	c := e.dialTUI()
	if res := resultMap(t, c.call("r1", "session.resume", map[string]any{keyLatest: sourceTUI})); res[keySessionID] != liveSession {
		t.Fatalf("result %+v", res)
	}
	// web-chat is checked once, though both listings name it.
	probes := 0
	for _, m := range e.hub.receivedMethods() {
		if m.Method == "session.info" && m.Params[keySessionID] == webChat {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("web-chat checked %d times", probes)
	}
}

func TestResumeLatestLookupFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.mu.Lock()
	e.hub.stored = []string{latestSession}
	e.hub.lookupFail = true
	e.hub.mu.Unlock()
	c := e.dialTUI()
	m := c.call("r1", "session.resume", map[string]any{})
	if m.Error == nil || m.Error.Data[keyReason] != splice.ReasonBind || m.Error.Data["hub_error"] != hubBackendDown {
		t.Fatalf("answer %+v", m)
	}
	if ids := e.hub.resumeIDs(); len(ids) != 0 {
		t.Fatalf("resumes %v", ids)
	}
}

func TestResumeByTitle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.mu.Lock()
	e.hub.titles[parserTitle] = parserSession
	e.hub.mu.Unlock()
	c := e.dialTUI()
	// The Ink TUI sends a title as session_id (/resume <title>, or a
	// --resume the launcher could not match locally); other clients send
	// a title member.
	for i, params := range []map[string]any{
		{keySessionID: parserTitle},
		{keyTitle: parserTitle},
	} {
		if res := resultMap(t, c.call(fmt.Sprintf("r%d", i), "session.resume", params)); res[keySessionID] != parserSession {
			t.Fatalf("resume %v: result %+v", params, res)
		}
	}
	if b := e.hub.bindCalls(); len(b) != 1 || b[0] != (bindCall{parserSession, deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
	got, _ := e.hub.find("session.resume", nil)
	if _, has := got.Params[keyTitle]; has {
		t.Fatalf("resume params %+v", got.Params)
	}
	m := c.call("r9", "session.resume", map[string]any{keyTitle: "no such chat"})
	if m.Error == nil || m.Error.Data[keyReason] != splice.ReasonNotFound {
		t.Fatalf("answer %+v", m)
	}
	if ids := e.hub.resumeIDs(); len(ids) != 2 {
		t.Fatalf("resumes %v", ids)
	}
}

func TestLookupEndpoint(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.mu.Lock()
	e.hub.titles[parserTitle] = parserSession
	e.hub.stored = []string{webChat, latestSession}
	e.hub.notMine[webChat] = reasonNotBound
	e.hub.mu.Unlock()
	secret := e.splice.ControlSecret()
	lookup := func(body string) (int, map[string]string) {
		resp := e.controlBody(http.MethodPost, splice.PathControlLookup, secret, body)
		out := map[string]string{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, out := lookup(`{"latest": true}`); code != http.StatusOK || out[keySessionID] != latestSession {
		t.Fatalf("latest: %d %v", code, out)
	}
	if code, out := lookup(`{"session": "` + parserTitle + `"}`); code != http.StatusOK || out[keySessionID] != parserSession {
		t.Fatalf("title: %d %v", code, out)
	}
	if code, out := lookup(`{"session": "nothing"}`); code != http.StatusNotFound || out[keyError] != "not_found" {
		t.Fatalf("missing: %d %v", code, out)
	}
	for _, body := range []string{`{}`, `{"session": ""}`, `{"session": "x", "latest": true}`, `[`} {
		if code, _ := lookup(body); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, code)
		}
	}
	if resp := e.controlBody(http.MethodPost, splice.PathControlLookup, "wrong", `{"latest": true}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong secret: %d", resp.StatusCode)
	}
	e.hub.mu.Lock()
	e.hub.lookupFail = true
	e.hub.mu.Unlock()
	if code, out := lookup(`{"latest": true}`); code != http.StatusBadGateway || out[keyError] != hubBackendDown {
		t.Fatalf("lookup failure: %d %v", code, out)
	}
	// A lookup binds and resumes nothing.
	if b := e.hub.bindCalls(); len(b) != 0 {
		t.Fatalf("binds %+v", b)
	}
	if ids := e.hub.resumeIDs(); len(ids) != 0 {
		t.Fatalf("resumes %v", ids)
	}
}

func TestCaseVariantMembers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	c.send(map[string]any{keyJSONRPC: jsonRPC, "id": "r1", keyMethod: "shell.exec", "METHOD": "test.echo"})
	if m := c.read(); m.Error == nil || m.Error.Code != splice.CodeDenied {
		t.Fatalf("answer %+v", m)
	}
}

func TestPingTimeoutClosesHub(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{PingInterval: 50 * time.Millisecond, PingTimeout: 100 * time.Millisecond})
	c := e.dialTUI()
	c.call("r1", "test.echo", map[string]any{})
	stall := make(chan struct{})
	e.hub.mu.Lock()
	e.hub.stall = stall
	e.hub.mu.Unlock()
	t.Cleanup(func() { close(stall) })
	// The gateway blocks before its next read, at the latest after it
	// answered this echo, so later pings get no pong.
	c.request("r2", "test.echo", map[string]any{})
	if m, err := c.readTimeout(10 * time.Second); err == nil && string(m.ID) != `"r2"` {
		t.Fatalf("unexpected frame %+v", m)
	} else if err != nil {
		if websocket.CloseStatus(err) != websocket.StatusInternalError {
			t.Fatalf("after the missed pongs: %v", err)
		}
		return
	}
	if _, err := c.readTimeout(10 * time.Second); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("after the missed pongs: %v", err)
	}
}
