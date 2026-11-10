package splice_test

import (
	"encoding/json"
	"errors"
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
	if got.Params["cwd"] != placeholder || got.Params["row_cwd"] != deviceDir || got.Params["request_id"] != "req-1" ||
		got.Params["source"] != "tui" || got.Params["cwd_explicit"] != true || got.Params["client_env"] == nil {
		t.Fatalf("gateway got %+v", got.Params)
	}
	// Other traffic flows while the create is held.
	c.call("r2", "test.echo", map[string]any{})
	if m, err := c.readTimeout(200 * time.Millisecond); !errors.Is(err, errNoFrame) {
		t.Fatalf("the TUI got %+v (%v) before the bind finished", m, err)
	}
	close(gate)
	m := c.read()
	if string(m.ID) != `"r1"` || resultMap(t, m)["session_id"] != "s-req-1" {
		t.Fatalf("answer %+v", m)
	}
	if b := e.hub.bindCalls(); len(b) != 1 || b[0] != (bindCall{"s-req-1", deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
}

func TestCreateBindFailureDeletes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	e.hub.bindFail[deviceDir] = "device_offline"
	c := e.dialTUI()
	m := c.call("r1", "session.create", createParams("req-1", deviceDir))
	if m.Error == nil || m.Error.Code != splice.CodeDevice || m.Error.Data["reason"] != splice.ReasonBind ||
		m.Error.Data["hub_error"] != "device_offline" {
		t.Fatalf("answer %+v", m)
	}
	del, ok := e.hub.find("session.mutate", func(p map[string]any) bool { return p["operation"] == "delete" })
	if !ok {
		t.Fatal("no delete mutate")
	}
	var id string
	_ = json.Unmarshal(del.ID, &id)
	p := del.Params
	if p["session_id"] != "s-req-1" || p["request_id"] != "hermes-device-delete-s-req-1" ||
		p["expected_revision"] != float64(1) || p["expected_generation"] != float64(0) ||
		len(p["payload"].(map[string]any)) != 0 || len(id) < len(splice.OwnIDPrefix) || id[:len(splice.OwnIDPrefix)] != splice.OwnIDPrefix {
		t.Fatalf("delete %s %+v", del.ID, p)
	}
	// The splice consumed the delete's answer: the next frame is the echo.
	if res := resultMap(t, c.call("r2", "test.echo", map[string]any{})); res["method"] != "test.echo" {
		t.Fatalf("next answer %+v", res)
	}
}

func TestCreateRetryBindsOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	a := resultMap(t, c.call("r1", "session.create", createParams("req-1", deviceDir)))
	b := resultMap(t, c.call("r2", "session.create", createParams("req-1", deviceDir)))
	if a["session_id"] != b["session_id"] {
		t.Fatalf("sessions %v and %v", a["session_id"], b["session_id"])
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
		if m.Error == nil || m.Error.Code != splice.CodeDevice || m.Error.Data["reason"] != splice.ReasonCwd {
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
	if m.Error == nil || m.Error.Data["reason"] != splice.ReasonBind || m.Error.Data["hub_error"] != "backend_unavailable" {
		t.Fatalf("answer %+v", m)
	}
	e.hub.mu.Lock()
	e.hub.whoamiFail = false
	e.hub.mu.Unlock()
	if res := resultMap(t, c.call("r2", "session.create", createParams("req-1", deviceDir))); res["cwd"] != placeholder {
		t.Fatalf("result %+v", res)
	}
}

func TestWorkspaceRebind(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.create", createParams("req-1", deviceDir)))
	other := "/home/alice/src/other"
	resultMap(t, c.call("r2", "session.mutate", map[string]any{"session_id": "s-req-1", "request_id": "m1",
		"expected_revision": 1, "expected_generation": 0, "operation": "workspace", "payload": map[string]any{"cwd": other}}))
	binds := e.hub.bindCalls()
	if len(binds) != 2 || binds[1] != (bindCall{"s-req-1", other}) {
		t.Fatalf("binds %+v", binds)
	}
	mut, _ := e.hub.find("session.mutate", nil)
	if mut.Params["payload"].(map[string]any)["cwd"] != placeholder || mut.Params["request_id"] != "m1" {
		t.Fatalf("gateway got %+v", mut.Params)
	}

	e.hub.mu.Lock()
	e.hub.bindFail["/home/alice/denied"] = "dir_refused"
	e.hub.mu.Unlock()
	m := c.call("r3", "session.mutate", map[string]any{"session_id": "s-req-1", "request_id": "m2",
		"operation": "workspace", "payload": map[string]any{"cwd": "/home/alice/denied"}})
	if m.Error == nil || m.Error.Data["hub_error"] != "dir_refused" {
		t.Fatalf("answer %+v", m)
	}
	if _, ok := e.hub.find("session.mutate", func(p map[string]any) bool { return p["request_id"] == "m2" }); ok {
		t.Fatal("a workspace move whose bind failed reached the gateway")
	}
	m = c.call("r4", "session.mutate", map[string]any{"session_id": "s-req-1", "operation": "workspace",
		"payload": map[string]any{"cwd": "relative"}})
	if m.Error == nil || m.Error.Data["reason"] != splice.ReasonCwd {
		t.Fatalf("answer %+v", m)
	}
}

func TestWorkspaceRefusedRestoresBinding(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.create", createParams("busy", deviceDir)))
	m := c.call("r2", "session.mutate", map[string]any{"session_id": "s-busy", "operation": "workspace",
		"payload": map[string]any{"cwd": "/home/alice/b"}})
	if m.Error == nil {
		t.Fatalf("answer %+v", m)
	}
	waitFor(t, "the restoring bind", func() bool { return len(e.hub.bindCalls()) == 3 })
	if b := e.hub.bindCalls(); b[1] != (bindCall{"s-busy", "/home/alice/b"}) || b[2] != (bindCall{"s-busy", deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
}

func TestBranchBindsChild(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.create", createParams("req-1", deviceDir)))
	res := resultMap(t, c.call("r2", "session.mutate", map[string]any{"session_id": "s-req-1", "request_id": "b1",
		"expected_revision": 1, "expected_generation": 0, "operation": "branch", "payload": map[string]any{}}))
	if res["branched_session_id"] != "child-s-req-1" {
		t.Fatalf("result %+v", res)
	}
	binds := e.hub.bindCalls()
	if len(binds) != 2 || binds[1] != (bindCall{"child-s-req-1", deviceDir}) {
		t.Fatalf("binds %+v", binds)
	}
	// A branch of a session this daemon never bound passes without a bind.
	resultMap(t, c.call("r3", "session.mutate", map[string]any{"session_id": "other", "operation": "branch", "payload": map[string]any{}}))
	if n := len(e.hub.bindCalls()); n != 2 {
		t.Fatalf("%d binds, want 2", n)
	}
	// A failed child bind still passes the answer.
	e.hub.mu.Lock()
	e.hub.bindFail[deviceDir] = "device_offline"
	e.hub.mu.Unlock()
	if res := resultMap(t, c.call("r4", "session.mutate", map[string]any{"session_id": "s-req-1", "operation": "branch",
		"payload": map[string]any{}})); res["branched_session_id"] != "child-s-req-1" {
		t.Fatalf("result %+v", res)
	}
}

func TestDeniedMethods(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	cases := []struct {
		method string
		params map[string]any
	}{
		{"config.set", map[string]any{"key": "cwd", "value": "/x"}},
		{"config.set", map[string]any{"key": "terminal.cwd", "value": "/x"}},
		{"config.set", map[string]any{}},
		{"vault.list", nil},
		{"vault.set", map[string]any{"name": "k"}},
		{"model.save_key", map[string]any{}},
		{"profiles.create", map[string]any{}},
		{"shell.exec", map[string]any{"command": "id"}},
		{"cli.exec", map[string]any{}},
		{"image.attach", map[string]any{"path": "/etc/passwd"}},
		{"pdf.attach", map[string]any{"path": "/x.pdf"}},
		{"file.attach", map[string]any{"path": "/x"}},
		{"input.detect_drop", map[string]any{}},
		{"clipboard.paste", map[string]any{}},
		{"mcp.servers.add", map[string]any{}},
		{"mcp.servers.test", map[string]any{}},
		{"plugins.manage", map[string]any{}},
		{"skills.manage", map[string]any{}},
		{"cron.manage", map[string]any{}},
		{"browser.manage", map[string]any{}},
		{"prompt.background", map[string]any{}},
		{"session.mutate", map[string]any{"session_id": "s", "operation": "editor_mcp", "payload": map[string]any{}}},
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
		if m.Error == nil || m.Error.Code != splice.CodeDenied || m.Error.Data["reason"] != splice.ReasonDenied ||
			m.Error.Message != tc.method+" is not available through hermes-device" {
			t.Errorf("%s %v: answer %+v", tc.method, tc.params, m)
		}
	}
	allowed := []struct {
		method string
		params map[string]any
	}{
		{"config.set", map[string]any{"key": "verbose", "value": "all", "session_id": "s"}},
		{"pdf.attach", map[string]any{"content_base64": "AA=="}},
		{"file.attach", map[string]any{"data_url": "data:,", "path": nil}},
		{"image.attach_bytes", map[string]any{"data": "AA=="}},
		{"slash.exec", map[string]any{"command": "/help"}},
		{"command.dispatch", map[string]any{"name": "help"}},
	}
	for i, tc := range allowed {
		if res := resultMap(t, c.call(100+i, tc.method, tc.params)); res["method"] != tc.method {
			t.Errorf("%s: result %+v", tc.method, res)
		}
	}
	for _, m := range e.hub.receivedMethods() {
		for _, tc := range cases {
			if m.Method == tc.method && m.Method != "session.mutate" && (m.Method != "config.set" || m.Params["key"] != "verbose") &&
				(m.Method != "pdf.attach" && m.Method != "file.attach" || m.Params["path"] != nil) {
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
	if res["session_id"] != "s-int-id" {
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
	c.send(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "result": map[string]any{"choice": "once"}})
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
	c.request("b1", "session.mutate", map[string]any{"session_id": "hold", "operation": "branch", "payload": map[string]any{}})
	c.request("c1", "session.create", createParams("hold", deviceDir))
	waitFor(t, "both held at the gateway", func() bool {
		_, a := e.hub.find("session.mutate", nil)
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
		if m.Error == nil || m.Error.Code != splice.CodeDevice || m.Error.Data["reason"] != splice.ReasonBypass {
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
	p["editor"] = map[string]any{"name": "zed", "mcp_servers": []any{map[string]any{"command": "x"}}}
	resultMap(t, c.call("r1", "session.create", p))
	got, _ := e.hub.find("session.create", nil)
	if ed := got.Params["editor"].(map[string]any); ed["name"] != "zed" || ed["mcp_servers"] != nil {
		t.Fatalf("editor at the gateway %+v", ed)
	}
	resultMap(t, c.call("r2", "session.mutate", map[string]any{"session_id": "s-req-1", "operation": "branch",
		"payload": map[string]any{"title": "t", "mcp_servers": []any{}}}))
	br, _ := e.hub.find("session.mutate", nil)
	if pl := br.Params["payload"].(map[string]any); pl["title"] != "t" || pl["mcp_servers"] != nil {
		t.Fatalf("branch payload at the gateway %+v", pl)
	}
	resultMap(t, c.call("r3", "session.resume", map[string]any{"session_id": "s-req-1",
		"editor": map[string]any{"mcp_servers": []any{}}}))
	rs, _ := e.hub.find("session.resume", nil)
	if ed := rs.Params["editor"].(map[string]any); len(ed) != 0 {
		t.Fatalf("resume editor at the gateway %+v", ed)
	}
}

func TestResumeBindsToChatDir(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	resultMap(t, c.call("r1", "session.resume", map[string]any{"session_id": "old"}))
	if b := e.hub.bindCalls(); len(b) != 1 || b[0] != (bindCall{"old", deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
	// A move, then a resume on the same chat (a reconnect), keeps the move.
	other := "/home/alice/src/other"
	resultMap(t, c.call("r2", "session.mutate", map[string]any{"session_id": "old", "operation": "workspace",
		"payload": map[string]any{"cwd": other}}))
	resultMap(t, c.call("r3", "session.resume", map[string]any{"session_id": "old"}))
	if b := e.hub.bindCalls(); len(b) != 2 || b[1] != (bindCall{"old", other}) {
		t.Fatalf("binds %+v", b)
	}
	// A latest resume binds the session the gateway picked.
	if res := resultMap(t, c.call("r4", "session.resume", map[string]any{"latest": "tui"})); res["session_id"] != "latest-s" {
		t.Fatalf("result %+v", res)
	}
	if b := e.hub.bindCalls(); len(b) != 3 || b[2] != (bindCall{"latest-s", deviceDir}) {
		t.Fatalf("binds %+v", b)
	}
	// A failed bind refuses the resume.
	e.hub.mu.Lock()
	e.hub.bindFail[deviceDir] = "bound_to_other_device"
	e.hub.mu.Unlock()
	m := c.call("r5", "session.resume", map[string]any{"session_id": "theirs"})
	if m.Error == nil || m.Error.Data["hub_error"] != "bound_to_other_device" {
		t.Fatalf("answer %+v", m)
	}
	if _, ok := e.hub.find("session.resume", func(p map[string]any) bool { return p["session_id"] == "theirs" }); ok {
		t.Fatal("a resume whose bind failed reached the gateway")
	}
}

func TestCaseVariantMembers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, splice.Options{})
	c := e.dialTUI()
	c.send(map[string]any{"jsonrpc": "2.0", "id": "r1", "method": "shell.exec", "METHOD": "test.echo"})
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
