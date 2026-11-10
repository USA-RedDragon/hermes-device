// Package devicetest has fakes of the Hermes side of hermes-device for
// tests: the device plugin, mounted on a gatewaytest.Gateway as the
// dashboard mounts it on the gateway listener, and acp-host. Both verify
// the hub assertion with the hermes-device assertion package.
package devicetest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
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
}

// NewPlugin mounts a fake plugin for backendID on gw. It accepts
// assertions signed by key.
func NewPlugin(gw *gatewaytest.Gateway, backendID string, key ed25519.PublicKey) *Plugin {
	p := &Plugin{BackendID: backendID, Placeholder: "/opt/data/hermes/device-placeholder", HermesVersion: "v0.22.7",
		verifier: assertion.NewVerifier([]ed25519.PublicKey{key}), bindings: map[string]Binding{}, ever: map[string]bool{},
		parents: map[string]string{}, conns: map[string]*Conn{}, refuse: map[string]string{}, Connected: make(chan *Conn, 16),
		trees: map[string]map[string][]string{}, refuseDev: map[string]string{}}
	base := "/api/plugins/device"
	gw.Handle("GET "+base+"/info", http.HandlerFunc(p.info), true)
	gw.Handle("GET "+base+"/devices", http.HandlerFunc(p.devices), true)
	gw.Handle("GET "+base+"/devices/{device}/dirs", http.HandlerFunc(p.dirs), true)
	gw.Handle("GET "+base+"/bindings", http.HandlerFunc(p.listBindings), true)
	gw.Handle("GET "+base+"/sessions/{session}/binding", http.HandlerFunc(p.getBinding), true)
	gw.Handle("PUT "+base+"/sessions/{session}/binding", http.HandlerFunc(p.putBinding), true)
	gw.Handle("DELETE "+base+"/sessions/{session}/binding", http.HandlerFunc(p.deleteBinding), true)
	gw.Handle("GET "+protocol.PluginExecPath, http.HandlerFunc(p.exec), false)
	return p
}

func pluginError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encode(w, map[string]string{"error": code, "message": code})
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
		answer(w, map[string]any{"path": dir, "entries": []map[string]string{{keyName: "src"}}})
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
	answer(w, map[string]any{"path": path.Clean(dir), "entries": entries})
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
			return b, map[string]any{stateKey: "bound", keyVia: cur}
		}
		ever = ever || p.ever[cur]
	}
	if ever {
		return Binding{}, map[string]any{stateKey: "fail_closed", keyVia: nil}
	}
	res := map[string]any{stateKey: "never_bound", keyVia: nil}
	if p.sandbox != "" {
		res["sandbox"] = nil
		if p.conns[p.sandbox] != nil {
			res["sandbox"] = map[string]string{keyDeviceID: p.sandbox, "dir": path.Join(p.sandboxRoot, sid)}
		}
	}
	return Binding{}, res
}

func (p *Plugin) getBinding(w http.ResponseWriter, r *http.Request) {
	p.record(r, nil)
	b, res := p.resolve(r.PathValue("session"))
	if res[stateKey] != "bound" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		encode(w, map[string]any{"error": "not_bound", "message": "not bound", "resolved": res})
		return
	}
	answer(w, map[string]any{"session_id": b.SessionID, keyDeviceID: b.DeviceID, "dir": b.Dir, "generation": b.Generation,
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
	dir, ok := body["dir"].(string)
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

// ACPHost is the fake acp-host: every connection is echoed back, as if a
// hermes acp process answered each line with itself.
type ACPHost struct {
	Server    *httptest.Server
	BackendID string
	verifier  *assertion.Verifier

	mu        sync.Mutex
	claims    []assertion.Claims
	closeWith websocket.StatusCode
}

// NewACPHost starts a fake acp-host for backendID.
func NewACPHost(backendID string, key ed25519.PublicKey) *ACPHost {
	h := &ACPHost{BackendID: backendID, verifier: assertion.NewVerifier([]ed25519.PublicKey{key})}
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
// message; 0 echoes again.
func (h *ACPHost) SetCloseWith(code websocket.StatusCode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeWith = code
}

// Claims returns the claims of every accepted connection.
func (h *ACPHost) Claims() []assertion.Claims {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.claims)
}

func (h *ACPHost) acp(w http.ResponseWriter, r *http.Request) {
	ws, c, ok := verify(w, r, h.verifier, h.BackendID, assertion.LegACP, nil)
	if !ok {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	h.mu.Lock()
	h.claims = append(h.claims, *c)
	closeWith := h.closeWith
	h.mu.Unlock()
	for {
		typ, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		if closeWith != 0 {
			_ = ws.Close(closeWith, "acp exited")
			return
		}
		if err := ws.Write(r.Context(), typ, data); err != nil {
			return
		}
	}
}
