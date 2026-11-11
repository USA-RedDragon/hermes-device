// Package devicetest has fakes of the Hermes side of hermes-device for
// tests: the device plugin, mounted on a gatewaytest.Gateway as the
// dashboard mounts it on the gateway listener, and acp-host. Both verify
// the hub assertion with the hermes-device assertion package.
package devicetest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/coder/websocket"
)

const (
	stateKey    = "state"
	keyVia      = "via"
	keyDeviceID = "device_id"
	keyName     = "name"
	keyPath     = "path"
	keyDir      = "dir"
	keyEntries  = "entries"
	stateBound  = "bound"
	stateNever  = "never_bound"
)

// Binding is a session binding of the fake plugin.
type Binding struct {
	SessionID  string  `json:"session_id"`
	DeviceID   string  `json:"device_id"`
	Dir        string  `json:"dir"`
	Generation string  `json:"generation"`
	BoundAt    float64 `json:"bound_at"`
}

// Conn is one device connected to the fake plugin's exec route.
type Conn struct {
	Claims assertion.Claims
	Hello  protocol.Hello
	ws     *websocket.Conn
	// Messages gets every message the device sent after hello.
	Messages chan json.RawMessage
}

// Send writes a message to the device.
func (c *Conn) Send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// Close closes the device's connection with code.
func (c *Conn) Close(code websocket.StatusCode, reason string) {
	_ = c.ws.Close(code, reason)
}

// Call is one HTTP request the fake plugin served.
type Call struct {
	Method, Path string
	Body         map[string]any
}

// Plugin is the fake device plugin.
type Plugin struct {
	BackendID     string
	Placeholder   string
	HermesVersion string
	verifier      *assertion.Verifier

	mu       sync.Mutex
	bindings map[string]Binding
	ever     map[string]bool
	parents  map[string]string
	conns    map[string]*Conn
	refuse   map[string]string
	// refuseDev makes binds to a device fail with a code.
	refuseDev map[string]string
	calls     []Call
	// trees are fake directory trees by device: each directory and its
	// subdirectories.
	trees map[string]map[string][]string
	// sandbox and sandboxRoot are the fallback device and fallback_root
	// of protocol.md section 8.10; "" is fallback local.
	sandbox, sandboxRoot string
	// Connected is sent every new exec connection.
	Connected chan *Conn
	// Refused counts refused assertions.
	refused int
	// uploads are the files the upload route wrote, by device path.
	uploads map[string][]byte
	// files are the files the read route serves, by device ID and path.
	files map[string]map[string][]byte
	// busyReads is how many reads still get the busy answer.
	busyReads int
	// diffs and changes are the answers of the diff and changes routes,
	// by session.
	diffs, changes map[string]map[string]any
}

// NewPlugin mounts a fake plugin for backendID on gw. It accepts
// assertions signed by key.
func NewPlugin(gw *gatewaytest.Gateway, backendID string, key ed25519.PublicKey) *Plugin {
	p := &Plugin{BackendID: backendID, Placeholder: "/opt/data/hermes/device-placeholder", HermesVersion: "v0.23.1",
		verifier: assertion.NewVerifier([]ed25519.PublicKey{key}), bindings: map[string]Binding{}, ever: map[string]bool{},
		parents: map[string]string{}, conns: map[string]*Conn{}, refuse: map[string]string{}, Connected: make(chan *Conn, 16),
		trees: map[string]map[string][]string{}, refuseDev: map[string]string{}, uploads: map[string][]byte{},
		files: map[string]map[string][]byte{}, diffs: map[string]map[string]any{}, changes: map[string]map[string]any{}}
	base := "/api/plugins/device"
	gw.Handle("GET "+base+"/info", http.HandlerFunc(p.info), true)
	gw.Handle("GET "+base+"/devices", http.HandlerFunc(p.devices), true)
	gw.Handle("GET "+base+"/devices/{device}/dirs", http.HandlerFunc(p.dirs), true)
	gw.Handle("GET "+base+"/bindings", http.HandlerFunc(p.listBindings), true)
	gw.Handle("GET "+base+"/sessions/{session}/binding", http.HandlerFunc(p.getBinding), true)
	gw.Handle("PUT "+base+"/sessions/{session}/binding", http.HandlerFunc(p.putBinding), true)
	gw.Handle("DELETE "+base+"/sessions/{session}/binding", http.HandlerFunc(p.deleteBinding), true)
	gw.Handle("POST "+base+"/sessions/{session}/files", http.HandlerFunc(p.postFile), true)
	gw.Handle("GET "+base+"/sessions/{session}/files", http.HandlerFunc(p.getFile), true)
	gw.Handle("GET "+base+"/sessions/{session}/tree", http.HandlerFunc(p.tree), true)
	gw.Handle("GET "+base+"/sessions/{session}/diff", http.HandlerFunc(p.diff), true)
	gw.Handle("GET "+base+"/sessions/{session}/changes", http.HandlerFunc(p.getChanges), true)
	gw.Handle("GET "+protocol.PluginExecPath, http.HandlerFunc(p.exec), false)
	return p
}

func pluginError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encode(w, map[string]string{keyError: code, keyMessage: code})
}

func encode(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	encode(w, v)
}

func (p *Plugin) record(r *http.Request, body map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, Call{Method: r.Method, Path: r.URL.Path, Body: body})
}

// Calls returns the HTTP requests served so far.
func (p *Plugin) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// Binds counts the PUT binding requests served so far.
func (p *Plugin) Binds() int {
	n := 0
	for _, c := range p.Calls() {
		if c.Method == http.MethodPut {
			n++
		}
	}
	return n
}

// Bindings returns every binding.
func (p *Plugin) Bindings() map[string]Binding {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]Binding{}
	for k, v := range p.bindings {
		out[k] = v
	}
	return out
}

// SetBinding binds a session as the plugin's own /device command would.
func (p *Plugin) SetBinding(sessionID, deviceID, dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bindings[sessionID] = Binding{SessionID: sessionID, DeviceID: deviceID, Dir: dir, Generation: gen(), BoundAt: 1}
	p.ever[sessionID] = true
}

// SetParent records a session's parent for lineage resolution.
func (p *Plugin) SetParent(child, parent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parents[child] = parent
}

// RefuseBinds makes every PUT binding of a session fail with code ("" ends it).
func (p *Plugin) RefuseBinds(sessionID, code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse[sessionID] = code
}

// Connect connects a device to the exec route without a WebSocket, as
// if it had sent hello, so its online state and roots show in /devices.
func (p *Plugin) Connect(deviceID, name string, hello protocol.Hello) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[deviceID] = &Conn{Claims: assertion.Claims{DeviceID: deviceID, DeviceName: name}, Hello: hello}
}

// Disconnect drops a device that Connect connected.
func (p *Plugin) Disconnect(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, deviceID)
}

// SetDirs gives a device a fake directory tree: each directory and its
// subdirectories. The dirs route then refuses any other directory with
// dir_refused, and so does a bind.
func (p *Plugin) SetDirs(deviceID string, tree map[string][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trees[deviceID] = tree
}

// SetSandbox turns on the sandbox fallback with device id and root.
func (p *Plugin) SetSandbox(deviceID, root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sandbox, p.sandboxRoot = deviceID, root
}

// RefuseBindsTo makes every PUT binding to a device fail with code (""
// ends it).
func (p *Plugin) RefuseBindsTo(deviceID, code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuseDev[deviceID] = code
}

// Conn returns a connected device.
func (p *Plugin) Conn(deviceID string) *Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[deviceID]
}

// RefusedAssertions counts refused exec upgrades.
func (p *Plugin) RefusedAssertions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused
}

func gen() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p *Plugin) info(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	answer(w, map[string]any{"plugin_version": PluginVersion, "hermes_version": p.HermesVersion, "protocol_version": protocol.Version,
		"backend_id": p.BackendID, "placeholder": p.Placeholder})
}

func (p *Plugin) devices(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	p.mu.Lock()
	out := make([]map[string]any, 0, len(p.conns))
	for id, c := range p.conns {
		out = append(out, map[string]any{keyDeviceID: id, keyName: c.Claims.DeviceName, "hostname": c.Hello.Hostname,
			"os": c.Hello.OS, "arch": c.Hello.Arch, "home": c.Hello.Home, "roots": c.Hello.Roots,
			"client_version": c.Hello.ClientVersion, "connected_at": 1})
	}
	p.mu.Unlock()
	answer(w, map[string]any{"devices": out})
}

func (p *Plugin) dirs(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	id := r.PathValue("device")
	c := p.Conn(id)
	if c == nil {
		pluginError(w, http.StatusConflict, "device_offline")
		return
	}
	dir := r.URL.Query().Get("path")
	p.mu.Lock()
	tree, ok := p.trees[id]
	p.mu.Unlock()
	if !ok {
		if dir == "" {
			dir = "/home/alice"
		}
		answer(w, map[string]any{keyPath: dir, keyEntries: []map[string]string{{keyName: "src"}}})
		return
	}
	if dir == "" && len(c.Hello.Roots) > 0 {
		dir = c.Hello.Roots[0]
	}
	if !path.IsAbs(dir) {
		pluginError(w, http.StatusBadRequest, "bad_dir")
		return
	}
	subs, ok := tree[path.Clean(dir)]
	if !ok {
		pluginError(w, http.StatusUnprocessableEntity, "dir_refused")
		return
	}
	entries := make([]map[string]string, 0, len(subs))
	for _, n := range subs {
		entries = append(entries, map[string]string{keyName: n})
	}
	answer(w, map[string]any{keyPath: path.Clean(dir), keyEntries: entries})
}

func (p *Plugin) listBindings(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	p.mu.Lock()
	out := make([]Binding, 0, len(p.bindings))
	for _, b := range p.bindings {
		out = append(out, b)
	}
	p.mu.Unlock()
	answer(w, map[string]any{"bindings": out})
}

// resolve walks a session's lineage to the nearest binding.
func (p *Plugin) resolve(sid string) (Binding, map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ever := false
	for cur, hops := sid, 0; cur != "" && hops < 100; cur, hops = p.parents[cur], hops+1 {
		if b, ok := p.bindings[cur]; ok {
			return b, map[string]any{stateKey: stateBound, keyVia: cur}
		}
		ever = ever || p.ever[cur]
	}
	if ever {
		return Binding{}, map[string]any{stateKey: "fail_closed", keyVia: nil}
	}
	res := map[string]any{stateKey: stateNever, keyVia: nil}
	if p.sandbox != "" {
		res["sandbox"] = nil
		if p.conns[p.sandbox] != nil {
			res["sandbox"] = map[string]string{keyDeviceID: p.sandbox, keyDir: path.Join(p.sandboxRoot, sid)}
		}
	}
	return Binding{}, res
}

func (p *Plugin) getBinding(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	b, res := p.resolve(r.PathValue("session"))
	if res[stateKey] != stateBound {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		encode(w, map[string]any{keyError: "not_bound", keyMessage: "not bound", "resolved": res})
		return
	}
	answer(w, map[string]any{"session_id": b.SessionID, keyDeviceID: b.DeviceID, keyDir: b.Dir, "generation": b.Generation,
		"bound_at": b.BoundAt, "resolved": res})
}

func (p *Plugin) putBinding(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		pluginError(w, http.StatusBadRequest, "bad_request")
		return
	}
	p.record(r, body)
	sid := r.PathValue("session")
	deviceID, _ := body[keyDeviceID].(string)
	dir, ok := body[keyDir].(string)
	if deviceID == "" || !ok {
		pluginError(w, http.StatusBadRequest, "bad_request")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if code := p.refuse[sid]; code != "" {
		pluginError(w, http.StatusConflict, code)
		return
	}
	if code := p.refuseDev[deviceID]; code != "" {
		pluginError(w, http.StatusConflict, code)
		return
	}
	if p.conns[deviceID] == nil {
		pluginError(w, http.StatusConflict, "device_offline")
		return
	}
	if !path.IsAbs(dir) {
		pluginError(w, http.StatusBadRequest, "bad_dir")
		return
	}
	if tree, ok := p.trees[deviceID]; ok {
		if _, ok := tree[path.Clean(dir)]; !ok {
			pluginError(w, http.StatusUnprocessableEntity, "dir_refused")
			return
		}
	} else if !strings.HasPrefix(dir, "/home/") {
		pluginError(w, http.StatusUnprocessableEntity, "dir_refused")
		return
	}
	old, had := p.bindings[sid]
	if had && old.DeviceID != deviceID {
		pluginError(w, http.StatusConflict, "bound_to_other_device")
		return
	}
	if had && old.Dir == dir {
		answer(w, old)
		return
	}
	b := Binding{SessionID: sid, DeviceID: deviceID, Dir: dir, Generation: gen(), BoundAt: float64(time.Now().Unix())}
	p.bindings[sid] = b
	p.ever[sid] = true
	answer(w, b)
}

func (p *Plugin) deleteBinding(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	p.mu.Lock()
	defer p.mu.Unlock()
	sid := r.PathValue("session")
	if _, ok := p.bindings[sid]; !ok {
		pluginError(w, http.StatusNotFound, "not_bound")
		return
	}
	delete(p.bindings, sid)
	w.WriteHeader(http.StatusNoContent)
}

// verify accepts the upgrade and checks its assertion, as the plugin and
// acp-host do: a refusal is a close with 4403 after the upgrade.
func verify(w http.ResponseWriter, r *http.Request, v *assertion.Verifier, aud, leg string, refused func()) (*websocket.Conn, *assertion.Claims, bool) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, nil, false
	}
	ws.SetReadLimit(64 << 20)
	c, err := v.Verify(r.Header.Get(assertion.Header), assertion.Expect{Audience: aud, Leg: leg, Method: r.Method, Path: r.URL.Path})
	if err != nil || r.Header.Get("Origin") != "" {
		if refused != nil {
			refused()
		}
		_ = ws.Close(protocol.CloseAssertionRefused, "assertion refused")
		return nil, nil, false
	}
	return ws, c, true
}

// Members of plugin and JSON-RPC error answers.
const (
	keyError   = "error"
	keyMessage = "message"
)

// PluginVersion is the release the fake plugin reports.
const PluginVersion = "0.3.0"

// negotiate picks the newest protocol version both the hello and the fake
// speak, as the plugin does (docs/protocol.md 4.1).
func negotiate(h protocol.Hello) (int, bool) {
	hi := h.MaxVersion
	if hi == 0 {
		hi = h.Version
	}
	v := min(hi, protocol.Version)
	return v, v >= max(h.Version, protocol.MinVersion)
}

func (p *Plugin) exec(w http.ResponseWriter, r *http.Request) {
	ws, claims, ok := verify(w, r, p.verifier, p.BackendID, assertion.LegExec, func() {
		p.mu.Lock()
		p.refused++
		p.mu.Unlock()
	})
	if !ok {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ctx := r.Context()
	hctx, cancel := context.WithTimeout(ctx, protocol.HelloTimeout)
	_, data, err := ws.Read(hctx)
	cancel()
	var hello protocol.Hello
	if err != nil || json.Unmarshal(data, &hello) != nil || hello.Type != protocol.TypeHello {
		_ = ws.Close(websocket.StatusProtocolError, "expected hello")
		return
	}
	version, ok := negotiate(hello)
	if !ok {
		_ = ws.Close(protocol.CloseUnsupportedVersion, fmt.Sprintf("hermes-device %s speaks protocol %d to %d; plugin %s speaks %d to %d; upgrade them together",
			hello.ClientVersion, hello.Version, max(hello.MaxVersion, hello.Version), PluginVersion, protocol.MinVersion, protocol.Version))
		return
	}
	c := &Conn{Claims: *claims, Hello: hello, ws: ws, Messages: make(chan json.RawMessage, 64)}
	p.mu.Lock()
	if old := p.conns[claims.DeviceID]; old != nil {
		go old.Close(protocol.CloseReplaced, "replaced")
	}
	p.conns[claims.DeviceID] = c
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.conns[claims.DeviceID] == c {
			delete(p.conns, claims.DeviceID)
		}
		p.mu.Unlock()
	}()
	welcome := protocol.Welcome{Type: protocol.TypeWelcome, Version: version, DeviceID: claims.DeviceID,
		Name: claims.DeviceName, PingInterval: int(protocol.PingInterval.Seconds()), PluginVersion: PluginVersion,
		HermesVersion: p.HermesVersion}
	if version >= protocol.ResumeVersion {
		welcome.GraceSeconds = protocol.DefaultGrace.Seconds()
	}
	_ = c.Send(ctx, welcome)
	if version >= protocol.ResumeVersion {
		_ = c.Send(ctx, protocol.Resume{Type: protocol.TypeResume, Commands: []protocol.ResumeCommand{}})
	}
	p.Connected <- c
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var env protocol.Envelope
		_ = json.Unmarshal(data, &env)
		if env.Type == protocol.TypePing {
			var ping struct {
				Seq int `json:"seq"`
			}
			_ = json.Unmarshal(data, &ping)
			_ = c.Send(ctx, map[string]any{"type": protocol.TypePong, "seq": ping.Seq})
			continue
		}
		select {
		case c.Messages <- data:
		default:
		}
	}
}

// ACPHost is the fake acp-host. Each connection acts as a small hermes
// acp: it answers initialize, the session methods and session/list itself,
// answers other requests with -32601, and records every message it gets.
type ACPHost struct {
	Server    *httptest.Server
	BackendID string
	verifier  *assertion.Verifier

	mu        sync.Mutex
	claims    []assertion.Claims
	closeWith websocket.StatusCode
	received  []map[string]any
	sessions  []map[string]any
	conn      *websocket.Conn
	nextID    int
	got       chan struct{}
}

// NewACPHost starts a fake acp-host for backendID.
func NewACPHost(backendID string, key ed25519.PublicKey) *ACPHost {
	h := &ACPHost{BackendID: backendID, verifier: assertion.NewVerifier([]ed25519.PublicKey{key}), got: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.ACPHostPath, h.acp)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.Server = httptest.NewServer(mux)
	return h
}

// URL is the fake's base URL.
func (h *ACPHost) URL() string { return h.Server.URL }

// Close stops the fake.
func (h *ACPHost) Close() { h.Server.Close() }

// SetCloseWith makes each new connection close with code after its first
// message; 0 answers again.
func (h *ACPHost) SetCloseWith(code websocket.StatusCode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeWith = code
}

// SetSessions sets the rows session/list answers with.
func (h *ACPHost) SetSessions(rows []map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = rows
}

// Claims returns the claims of every accepted connection.
func (h *ACPHost) Claims() []assertion.Claims {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.claims)
}

// Received returns every message the fake got, in order.
func (h *ACPHost) Received() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.received)
}

// WaitReceived waits until the fake got at least n messages.
func (h *ACPHost) WaitReceived(ctx context.Context, n int) []map[string]any {
	for {
		if got := h.Received(); len(got) >= n {
			return got
		}
		select {
		case <-h.got:
		case <-ctx.Done():
			return h.Received()
		}
	}
}

// Send sends msg to the device on the latest connection, as the agent.
func (h *ACPHost) Send(ctx context.Context, msg any) error {
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func (h *ACPHost) answer(m map[string]any) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := map[string]any{}
	switch m["method"] {
	case "initialize":
		result["protocolVersion"] = 1
	case "session/new", "session/fork":
		h.nextID++
		result["sessionId"] = fmt.Sprintf("acp-%d", h.nextID)
	case "session/list":
		result["sessions"] = h.sessions
	case "authenticate", "session/load", "session/resume", "session/prompt", "session/set_mode", "session/set_model",
		"session/set_config_option":
	default:
		return map[string]any{"jsonrpc": "2.0", "id": m["id"], keyError: map[string]any{"code": -32601, keyMessage: "Method not found"}}
	}
	return map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result}
}

func (h *ACPHost) acp(w http.ResponseWriter, r *http.Request) {
	ws, c, ok := verify(w, r, h.verifier, h.BackendID, assertion.LegACP, nil)
	if !ok {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	h.mu.Lock()
	h.claims = append(h.claims, *c)
	h.conn = ws
	closeWith := h.closeWith
	h.mu.Unlock()
	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		if closeWith != 0 {
			_ = ws.Close(closeWith, "acp exited")
			return
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		h.mu.Lock()
		h.received = append(h.received, m)
		h.mu.Unlock()
		select {
		case h.got <- struct{}{}:
		default:
		}
		_, isRequest := m["id"]
		if _, hasMethod := m["method"]; !hasMethod || !isRequest {
			continue
		}
		b, err := json.Marshal(h.answer(m))
		if err != nil {
			return
		}
		if err := ws.Write(r.Context(), websocket.MessageText, b); err != nil {
			return
		}
	}
}

// Uploads returns the files the upload route wrote, by device path.
func (p *Plugin) Uploads() map[string][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.uploads)
}

// SetDeviceFile puts a file on a device for the read route.
func (p *Plugin) SetDeviceFile(deviceID, filePath string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.files[deviceID] == nil {
		p.files[deviceID] = map[string][]byte{}
	}
	p.files[deviceID][filePath] = data
}

// BusyReads makes the next n reads get the plugin's busy answer.
func (p *Plugin) BusyReads(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busyReads = n
}

// sessionDevice is the binding that serves a session's files, as the
// upload and read rules say, or the error code of the plugin.
func (p *Plugin) sessionDevice(sid string) (Binding, string) {
	b, res := p.resolve(sid)
	switch {
	case res[stateKey] == stateBound:
	case res[stateKey] == stateNever && p.sandbox == "":
		return Binding{}, "runs_on_pod"
	case res[stateKey] == stateNever && res["sandbox"] != nil:
		sb, _ := res["sandbox"].(map[string]string)
		b = Binding{DeviceID: sb[keyDeviceID], Dir: sb[keyDir]}
	default:
		return Binding{}, "fail_closed"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[b.DeviceID] == nil {
		return Binding{}, "device_offline"
	}
	return b, ""
}

// getFile serves a file of the device that serves a session
// (docs/protocol.md section 9, read rules), from memory. Paths outside
// the device's roots are refused, and a file over 32 MiB is too large.
func (p *Plugin) getFile(w http.ResponseWriter, r *http.Request) {
	fp := r.URL.Query().Get(keyPath)
	p.record(r, map[string]any{keyPath: fp})
	p.mu.Lock()
	busy := p.busyReads > 0
	if busy {
		p.busyReads--
	}
	p.mu.Unlock()
	if busy {
		pluginError(w, http.StatusTooManyRequests, "busy")
		return
	}
	if !path.IsAbs(fp) || path.Clean(fp) != fp {
		pluginError(w, http.StatusBadRequest, "bad_path")
		return
	}
	b, code := p.sessionDevice(r.PathValue("session"))
	if code != "" {
		pluginError(w, http.StatusConflict, code)
		return
	}
	p.mu.Lock()
	inRoots := false
	for _, root := range p.conns[b.DeviceID].Hello.Roots {
		inRoots = inRoots || strings.HasPrefix(fp, root+"/")
	}
	data, ok := p.files[b.DeviceID][fp]
	p.mu.Unlock()
	switch {
	case !inRoots:
		pluginError(w, http.StatusUnprocessableEntity, "read_refused")
	case !ok:
		pluginError(w, http.StatusNotFound, "not_found")
	case len(data) > 32<<20:
		pluginError(w, http.StatusRequestEntityTooLarge, "too_large")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}
}

// postFile writes an upload as the plugin's upload rules say
// (docs/protocol.md section 9), into memory.
func (p *Plugin) postFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string `json:"name"`
		ContentBase64 string `json:"content_base64"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		pluginError(w, http.StatusBadRequest, "bad_request")
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.ContentBase64)
	if err != nil || body.Name == "" || strings.ContainsAny(body.Name, "/\\") {
		pluginError(w, http.StatusBadRequest, "bad_request")
		return
	}
	p.record(r, map[string]any{keyName: body.Name, "bytes": len(data)})
	b, code := p.sessionDevice(r.PathValue("session"))
	if code != "" {
		pluginError(w, http.StatusConflict, code)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	target := path.Join(b.Dir, ".hermes", "attachments", body.Name)
	p.uploads[target] = data
	answer(w, map[string]any{keyPath: target, keyName: body.Name, keyDeviceID: b.DeviceID, "bytes": len(data)})
}

// SetDiff sets the diff route's answer for a session.
func (p *Plugin) SetDiff(sessionID string, d map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.diffs[sessionID] = d
}

// SetChanges sets the changes route's answer for a session.
func (p *Plugin) SetChanges(sessionID string, c map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changes[sessionID] = c
}

// tree lists a folder of the device that serves a session from the files
// the read route serves (docs/protocol.md section 9, listing rules).
func (p *Plugin) tree(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get(keyPath)
	p.record(r, map[string]any{keyPath: dir})
	b, code := p.sessionDevice(r.PathValue("session"))
	if code != "" {
		pluginError(w, http.StatusConflict, code)
		return
	}
	if dir == "" {
		dir = b.Dir
	}
	if !path.IsAbs(dir) || path.Clean(dir) != dir {
		pluginError(w, http.StatusBadRequest, "bad_path")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	inRoots := false
	for _, root := range p.conns[b.DeviceID].Hello.Roots {
		inRoots = inRoots || dir == root || strings.HasPrefix(dir, root+"/")
	}
	if !inRoots {
		pluginError(w, http.StatusUnprocessableEntity, "read_refused")
		return
	}
	seen := map[string]bool{}
	entries := []map[string]any{}
	for fp, data := range p.files[b.DeviceID] {
		rest, ok := strings.CutPrefix(fp, strings.TrimSuffix(dir, "/")+"/")
		if !ok {
			continue
		}
		name, sub, deeper := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		if deeper && sub != "" {
			entries = append(entries, map[string]any{keyName: name, "kind": keyDir, "link": false, "size": 0})
		} else {
			entries = append(entries, map[string]any{keyName: name, "kind": "file", "link": false, "size": len(data)})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return fmt.Sprint(entries[i][keyName]) < fmt.Sprint(entries[j][keyName]) })
	answer(w, map[string]any{keyPath: dir, keyDir: b.Dir, keyDeviceID: b.DeviceID, keyEntries: entries, "truncated": false})
}

// diff answers with the session's diff set by SetDiff, or no git.
func (p *Plugin) diff(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	b, code := p.sessionDevice(r.PathValue("session"))
	if code != "" {
		pluginError(w, http.StatusConflict, code)
		return
	}
	p.mu.Lock()
	d, ok := p.diffs[r.PathValue("session")]
	p.mu.Unlock()
	if ok {
		d = maps.Clone(d)
		d[keyDeviceID] = b.DeviceID
	} else {
		d = map[string]any{keyDir: b.Dir, keyDeviceID: b.DeviceID, "git": false, "files": []any{}, "untracked": []any{}, "diff": ""}
	}
	answer(w, d)
}

// getChanges answers with the session's written files set by SetChanges.
func (p *Plugin) getChanges(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	p.mu.Lock()
	c, ok := p.changes[r.PathValue("session")]
	p.mu.Unlock()
	if !ok {
		c = map[string]any{"session_id": r.PathValue("session"), "files": []any{}}
	}
	answer(w, c)
}
