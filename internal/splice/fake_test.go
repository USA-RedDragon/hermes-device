package splice_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/coder/websocket"
)

const (
	hubToken    = "hdt_test"
	placeholder = "/opt/data/hermes/device-placeholder"
	deviceDir   = "/home/alice/src/app"
)

const (
	jsonRPC = "2.0"

	keyCommand     = "command"
	keyConfigKey   = "key"
	keyConfigValue = "value"
	keyCwd         = "cwd"
	keyError       = "error"
	keyJSONRPC     = "jsonrpc"
	keyMCPServers  = "mcp_servers"
	keyMethod      = "method"
	keyName        = "name"
	keyOperation   = "operation"
	keyParams      = "params"
	keyPath        = "path"
	keyPayload     = "payload"
	keyRequestID   = "request_id"
	keyRevision    = "revision"
	keySessionID   = "session_id"

	methodConfigSet  = "config.set"
	methodFileAttach = "file.attach"
	methodMutate     = "session.mutate"
	methodPDFAttach  = "pdf.attach"

	opBranch    = "branch"
	opWorkspace = "workspace"

	keyCode      = "code"
	keyData      = "data"
	keyMessage   = "message"
	keyReason    = "reason"
	keyTitle     = "title"
	keyScope     = "scope"
	keyLatest    = "latest"
	keyReasoning = "reasoning"
	scopeGlobal  = "global"

	hubBackendDown = "backend_unavailable"
	reasonNotBound = "hermes_device_not_bound"
	webChat        = "web-chat"
	liveSession    = "live-1"
	parserSession  = "s-parser"
	parserTitle    = "fix the parser"

	sourceTUI  = "tui"
	hubOffline = "device_offline"

	holdID        = "hold"
	sessionReq1   = "s-req-1"
	childReq1     = "child-s-req-1"
	busySession   = "s-busy"
	latestSession = "latest-s"
	oldSession    = "old"
)

// msg is a decoded JSON-RPC message.
type msg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params map[string]any  `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error,omitempty"`
}

type bindCall struct {
	SessionID string
	Dir       string
}

// fakeHub serves the hub's device API: whoami, session bindings and a
// gateway leg that speaks the canonical JSON-RPC shapes of Hermes v0.23.1.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	whoamiCalls int
	whoamiFail  bool
	binds       []bindCall
	// bindFail maps a directory to the hub error code its bind gets.
	bindFail map[string]string
	// bindGate, when set, blocks every bind until it is closed.
	bindGate chan struct{}
	received []msg
	legs     []*websocket.Conn
	// stall makes the gateway stop reading, so pings go unanswered.
	stall chan struct{}
	// titles maps a session title to its session id for session.list.
	titles map[string]string
	// stored and live are the session ids of the stored and live
	// session.list rows, most recent first.
	stored, live []string
	// notMine maps a session id to the data.reason the hub refuses it with.
	notMine map[string]string
	// lookupFail makes the hub fail every binding lookup.
	lookupFail bool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{t: t, bindFail: map[string]string{}, titles: map[string]string{}, notMine: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+hubapi.PathWhoami, h.whoami)
	mux.HandleFunc("PUT /api/device/sessions/{id}/binding", h.bind)
	mux.HandleFunc("GET "+hubapi.PathGatewayLeg, h.gateway)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+hubToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func (h *fakeHub) whoami(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	h.mu.Lock()
	h.whoamiCalls++
	fail := h.whoamiFail
	h.mu.Unlock()
	if fail {
		writeJSON(w, http.StatusBadGateway, map[string]string{keyError: hubBackendDown})
		return
	}
	writeJSON(w, http.StatusOK, hubapi.Whoami{DeviceID: "d_1", DeviceName: "desktop", User: "alice",
		Backend: "alice", HermesVersion: "v0.22.7", PluginVersion: "0.2.0", Placeholder: placeholder})
}

func (h *fakeHub) bind(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var body struct {
		Dir string `json:"dir"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	h.mu.Lock()
	gate := h.bindGate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	sid := r.PathValue("id")
	h.mu.Lock()
	h.binds = append(h.binds, bindCall{SessionID: sid, Dir: body.Dir})
	code := h.bindFail[body.Dir]
	h.mu.Unlock()
	if code != "" {
		writeJSON(w, http.StatusConflict, map[string]string{keyError: code, keyMessage: "refused"})
		return
	}
	writeJSON(w, http.StatusOK, hubapi.Binding{SessionID: sid, DeviceID: "d_1", Dir: body.Dir, Generation: "g1"})
}

func (h *fakeHub) bindCalls() []bindCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bindCall(nil), h.binds...)
}

func (h *fakeHub) receivedMethods() []msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]msg(nil), h.received...)
}

func (h *fakeHub) dropLegs() {
	h.mu.Lock()
	legs := h.legs
	h.mu.Unlock()
	for _, l := range legs {
		_ = l.CloseNow()
	}
}

func (h *fakeHub) gateway(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.legs = append(h.legs, ws)
	h.mu.Unlock()
	ctx := r.Context()
	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			h.t.Errorf("fake hub: encode %+v: %v", v, err)
			return
		}
		_ = ws.Write(ctx, websocket.MessageText, b)
	}
	answer := func(id json.RawMessage, result any) {
		send(map[string]any{keyJSONRPC: jsonRPC, "id": id, "result": result})
	}
	send(map[string]any{keyJSONRPC: jsonRPC, keyMethod: "event", keyParams: map[string]any{
		"type": "gateway.ready", keyPayload: map[string]any{"session_authority": true}}})
	waiting := map[string]json.RawMessage{}
	for {
		h.mu.Lock()
		stall := h.stall
		h.mu.Unlock()
		if stall != nil {
			<-stall
		}
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var m msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		h.mu.Lock()
		h.received = append(h.received, m)
		h.mu.Unlock()
		if m.Method == "" {
			var sid string
			_ = json.Unmarshal(m.ID, &sid)
			if orig, ok := waiting[sid]; ok {
				delete(waiting, sid)
				answer(orig, map[string]any{"answer": m.Result})
			}
			continue
		}
		p := m.Params
		switch m.Method {
		case "session.create":
			rid, _ := p[keyRequestID].(string)
			if rid == holdID {
				continue
			}
			answer(m.ID, map[string]any{keySessionID: "s-" + rid, keyRevision: 1, "execution_generation": 0,
				keyCwd: p[keyCwd]})
		case methodMutate:
			sid, _ := p[keySessionID].(string)
			if sid == holdID {
				continue
			}
			res := map[string]any{keySessionID: sid, keyOperation: p[keyOperation], keyRevision: 2}
			switch p[keyOperation] {
			case opBranch:
				res["branched_session_id"] = "child-" + sid
			case opWorkspace:
				if strings.HasPrefix(sid, busySession) {
					send(map[string]any{keyJSONRPC: jsonRPC, "id": m.ID,
						keyError: map[string]any{keyCode: 4009, keyMessage: "session_busy"}})
					continue
				}
			}
			answer(m.ID, res)
		case "session.resume":
			// hermes-hub v0.5.0 refuses a resume by title or latest, or
			// without a session_id.
			sid, _ := p[keySessionID].(string)
			_, title := p[keyTitle]
			_, latest := p[keyLatest]
			if sid == "" || title || latest {
				send(map[string]any{keyJSONRPC: jsonRPC, "id": m.ID, keyError: map[string]any{keyCode: -32601,
					keyMessage: "session.resume is not available through talaria", keyData: map[string]any{
						keyReason: "hermes_device_denied"}}})
				continue
			}
			answer(m.ID, map[string]any{keySessionID: sid, keyRevision: 3, keyParams: p})
		case "session.list":
			answer(m.ID, map[string]any{"sessions": h.listRows(p)})
		case "session.info":
			sid, _ := p[keySessionID].(string)
			h.mu.Lock()
			reason, refused := h.notMine[sid]
			fail := h.lookupFail
			h.mu.Unlock()
			switch {
			case fail:
				send(map[string]any{keyJSONRPC: jsonRPC, "id": m.ID, keyError: map[string]any{keyCode: 4001,
					keyMessage: "plugin down", keyData: map[string]any{keyReason: "hermes_device_bind",
						"hub_error": hubBackendDown}}})
			case refused:
				send(map[string]any{keyJSONRPC: jsonRPC, "id": m.ID, keyError: map[string]any{keyCode: 4001,
					keyMessage: "refused", keyData: map[string]any{keyReason: reason}}})
			default:
				send(map[string]any{keyJSONRPC: jsonRPC, "id": m.ID, keyError: map[string]any{keyCode: -32602,
					keyMessage: "invalid_params"}})
			}
		case "test.server_request":
			sid := fmt.Sprintf("srv-%d", len(waiting)+1)
			waiting[sid] = m.ID
			send(map[string]any{keyJSONRPC: jsonRPC, "id": sid, keyMethod: "approval.request",
				keyParams: map[string]any{"question": "ok?"}})
		default:
			answer(m.ID, map[string]any{keyMethod: m.Method, keyParams: p})
		}
	}
}

// listRows answers session.list as the gateway does: one row for a title
// or session id, else the stored or live rows.
func (h *fakeHub) listRows(p map[string]any) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows := []map[string]any{}
	if title, ok := p[keyTitle].(string); ok {
		if sid, ok := h.titles[title]; ok {
			rows = append(rows, map[string]any{keySessionID: sid, "id": sid, "resolved_id": sid, keyTitle: title})
		}
		return rows
	}
	ids := h.live
	if p[keyScope] == "stored" {
		ids = h.stored
	}
	for _, sid := range ids {
		rows = append(rows, map[string]any{keySessionID: sid, "id": sid})
	}
	return rows
}

// env is a splice serving in front of a fake hub.
type env struct {
	t      *testing.T
	hub    *fakeHub
	splice *splice.Server
	addr   string
}

func newEnv(t *testing.T, opts splice.Options) *env {
	t.Helper()
	h := newFakeHub(t)
	opts.Hub = hubapi.New(h.srv.URL, hubToken, "test")
	s := splice.New(opts)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return &env{t: t, hub: h, splice: s, addr: ln.Addr().String()}
}

func (e *env) control(method, path, secret string) *http.Response {
	e.t.Helper()
	return e.controlBody(method, path, secret, `{"cwd": "`+deviceDir+`"}`)
}

func (e *env) controlBody(method, path, secret, body string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), method, "http://"+e.addr+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (e *env) newToken() string {
	e.t.Helper()
	resp := e.control(http.MethodPost, splice.PathControlTokens, e.splice.ControlSecret())
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("POST control/tokens: %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Token) != 43 {
		e.t.Fatalf("token %q: %v", out.Token, err)
	}
	return out.Token
}

// tui is a fake Ink TUI client. A reader goroutine queues every frame,
// because a Read whose context ends closes a coder/websocket connection.
type tui struct {
	t      *testing.T
	ws     *websocket.Conn
	frames chan []byte
	err    chan error
}

func (e *env) dialTUI() *tui {
	e.t.Helper()
	return e.dialTUIToken(e.newToken())
}

func (e *env) dialTUIToken(token string) *tui {
	e.t.Helper()
	ws, resp, err := websocket.Dial(e.t.Context(), "ws://"+e.addr+splice.PathTUI+"?token="+token, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		e.t.Fatal(err)
	}
	ws.SetReadLimit(1 << 24)
	e.t.Cleanup(func() { _ = ws.CloseNow() })
	c := &tui{t: e.t, ws: ws, frames: make(chan []byte, 64), err: make(chan error, 1)}
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				c.err <- err
				return
			}
			c.frames <- data
		}
	}()
	first := c.read()
	if first.Method != "event" || first.Params["type"] != "gateway.ready" {
		e.t.Fatalf("first frame %+v, want gateway.ready", first)
	}
	return c
}

func (c *tui) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.ws.Write(c.t.Context(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *tui) request(id any, method string, params any) {
	c.t.Helper()
	c.send(map[string]any{keyJSONRPC: jsonRPC, "id": id, keyMethod: method, keyParams: params})
}

var errNoFrame = errors.New("no frame in time")

// readTimeout returns the next frame, the socket's read error, or
// errNoFrame after d.
func (c *tui) readTimeout(d time.Duration) (msg, error) {
	select {
	case data := <-c.frames:
		var m msg
		if err := json.Unmarshal(data, &m); err != nil {
			c.t.Fatalf("bad frame %s: %v", data, err)
		}
		return m, nil
	case err := <-c.err:
		c.err <- err
		return msg{}, err
	case <-time.After(d):
		return msg{}, errNoFrame
	}
}

func (c *tui) read() msg {
	c.t.Helper()
	m, err := c.readTimeout(10 * time.Second)
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

// call sends a request and returns its answer.
func (c *tui) call(id any, method string, params any) msg {
	c.t.Helper()
	c.request(id, method, params)
	m := c.read()
	want, err := json.Marshal(id)
	if err != nil {
		c.t.Fatal(err)
	}
	if string(m.ID) != string(want) {
		c.t.Fatalf("answer id %s, want %s (%+v)", m.ID, want, m)
	}
	return m
}

func createParams(requestID, cwd string) map[string]any {
	p := map[string]any{keyRequestID: requestID, "source": sourceTUI, "cwd_explicit": true,
		"client_env": map[string]string{"PATH": "/usr/bin"}}
	if cwd != "" {
		p[keyCwd] = cwd
	}
	return p
}

func resultMap(t *testing.T, m msg) map[string]any {
	t.Helper()
	if m.Error != nil {
		t.Fatalf("error answer: %+v", m.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(m.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *fakeHub) find(method string, match func(map[string]any) bool) (msg, bool) {
	for _, m := range h.receivedMethods() {
		if m.Method == method && (match == nil || match(m.Params)) {
			return m, true
		}
	}
	return msg{}, false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// asMap returns v as a JSON object, failing the test when it is not one.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v (%T) is not a JSON object", v, v)
	}
	return m
}
