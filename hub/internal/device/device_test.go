package device_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/device/devicetest"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/store/storetest"
	"github.com/coder/websocket"
)

const (
	userAlice   = "alice"
	backendID   = "alice-pc"
	deviceName  = "desktop"
	deviceDir   = "/home/alice/src"
	placeholder = "/opt/data/hermes/device-placeholder"

	providerBasic   = "basic"
	keyName         = "name"
	archAMD64       = "amd64"
	keyDeviceCode   = "device_code"
	keyUserCode     = "user_code"
	keyToken        = "token"
	keyDeviceID     = "device_id"
	keyCWD          = "cwd"
	keyDir          = "dir"
	keyReasoning    = "reasoning"
	keyOperation    = "operation"
	keyRequestID    = "request_id"
	methodConfigSet = "config.set"
	jsonRPC         = "2.0"
	keyJSONRPC      = "jsonrpc"
	keyKey          = "key"
	keyScope        = "scope"
	keyPicker       = "picker"
	scopeStored     = "stored"
	keyPayload      = "payload"
	opWorkspace     = "workspace"
	hubUser         = "hub"
	hostName        = "desktop"
	osLinux         = "linux"
	keyHostname     = "hostname"
	keyMethod       = "method"
	keyValue        = "value"
	keySessionID    = "session_id"
	otherDir        = "/home/alice/other"
)

// access is the hub's backend authorization, switchable by tests.
type access struct{ denied atomic.Bool }

func (a *access) Allowed(_ context.Context, user, backend string) error {
	if a.denied.Load() || user != userAlice || backend != backendID {
		return hub.ErrForbidden
	}
	return nil
}

// replica is one hub process: its own device server over a shared store.
type replica struct {
	dev *device.Server
	url string
}

type env struct {
	t      *testing.T
	gw     *gatewaytest.Gateway
	plugin *devicetest.Plugin
	acp    *devicetest.ACPHost
	st     *store.Store
	key    ed25519.PrivateKey
	access *access
	svc    *service.Service
	a, b   *replica
}

// stores runs fn against an SQLite store and, when storetest.DSNEnv is
// set, a PostgreSQL store.
func stores(t *testing.T, fn func(t *testing.T, st *store.Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		fn(t, st)
	})
	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		st, err := store.OpenPostgres(context.Background(), storetest.Postgres(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		fn(t, st)
	})
}

func setup(t *testing.T, st *store.Store) *env {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	pub, _ := key.Public().(ed25519.PublicKey)
	gw := gatewaytest.New(hubUser, "pw")
	t.Cleanup(gw.Close)
	acp := devicetest.NewACPHost(backendID, pub)
	t.Cleanup(acp.Close)
	e := &env{t: t, gw: gw, plugin: devicetest.NewPlugin(gw, backendID, pub), acp: acp, st: st, key: key, access: &access{}}
	e.svc = service.New([]config.Backend{e.backend()}, nil, service.Options{})
	t.Cleanup(e.svc.Close)
	e.a, e.b = e.replica(), e.replica()
	return e
}

func (e *env) replica() *replica {
	t := e.t
	srv := httptest.NewUnstartedServer(nil)
	pub := "http://" + srv.Listener.Addr().String()
	b := e.backend()
	d, err := device.New(device.Options{Store: e.st, Access: e.access, Sessions: e.svc, Backends: []config.Backend{b},
		Dialers: map[string]device.Dialer{backendID: gateway.NewDialer(gateway.Credentials{BaseURL: b.URL, Provider: providerBasic,
			Username: hubUser, Password: "pw"})},
		Key: e.key, PublicURL: pub,
		Config:       config.Device{Enabled: true, PairingTTL: time.Minute, PollInterval: time.Second, RecheckInterval: 100 * time.Millisecond},
		PingInterval: 100 * time.Millisecond, PingTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = d.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(d.Drain)
	return &replica{dev: d, url: pub}
}

func (e *env) backend() config.Backend {
	return config.Backend{ID: backendID, Name: "Alice's PC", URL: e.gw.URL(), Provider: providerBasic, Username: hubUser, Password: "pw",
		Users: []string{userAlice}, Devices: true, ACPURL: e.acp.URL(),
		ServiceDevices: []config.ServiceDevice{{ID: sandboxID, Name: sandboxName, Token: sandboxToken}}}
}

func call(t *testing.T, method, u, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, u, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func str(t *testing.T, m map[string]any, k string) string {
	t.Helper()
	v, ok := m[k].(string)
	if !ok {
		t.Fatalf("%s = %v, not a string", k, m[k])
	}
	return v
}

// pair runs the whole pairing flow and returns the device token.
func (e *env) pair(r *replica) (string, string) {
	e.t.Helper()
	return e.pairAs(r, deviceName)
}

// pairAs pairs a device of alice's under name.
func (e *env) pairAs(r *replica, name string) (string, string) {
	t := e.t
	t.Helper()
	status, start := call(t, http.MethodPost, r.url+"/api/device/pair", "", map[string]string{keyName: name,
		keyHostname: hostName, "os": osLinux, "arch": archAMD64, "client_version": "v0.1.0"})
	if status != http.StatusOK || start["verification_uri"] != r.url+"/device/pair" {
		t.Fatalf("pair = %d %v", status, start)
	}
	if _, err := e.st.ApprovePairing(context.Background(), str(t, start, keyUserCode), userAlice, backendID, name); err != nil {
		t.Fatal(err)
	}
	status, res := call(t, http.MethodPost, r.url+"/api/device/pair/token", "", map[string]string{keyDeviceCode: str(t, start, keyDeviceCode)})
	if status != http.StatusOK {
		t.Fatalf("poll = %d %v", status, res)
	}
	return str(t, res, keyToken), str(t, res, keyDeviceID)
}

func dial(t *testing.T, r *replica, path, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, resp, err := websocket.Dial(ctx, gateway.WebSocketURL(r.url)+path, &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		c.SetReadLimit(64 << 20)
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

func readJSON(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("not JSON: %s", data)
	}
	return m
}

func writeJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func closeCode(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// connectExec opens the exec leg and finishes the handshake.
func (e *env) connectExec(r *replica, token string) (*websocket.Conn, *devicetest.Conn) {
	t := e.t
	t.Helper()
	c, _, err := dial(t, r, "/api/device/exec", token)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, c, protocol.Hello{Type: protocol.TypeHello, Version: protocol.MinVersion, MaxVersion: protocol.Version,
		Roots: []string{homeDir}, Home: homeDir, Hostname: hostName, OS: osLinux, Arch: archAMD64, Shell: "bash",
		ClientVersion: "v0.3.0"})
	if w := readJSON(t, c); w["type"] != protocol.TypeWelcome || w["version"] != float64(protocol.Version) {
		t.Fatalf("welcome = %v", w)
	}
	if r := readJSON(t, c); r["type"] != protocol.TypeResume {
		t.Fatalf("resume = %v", r)
	}
	select {
	case pc := <-e.plugin.Connected:
		return c, pc
	case <-time.After(5 * time.Second):
		t.Fatal("plugin saw no connection")
	}
	return nil, nil
}

func TestPairing(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		status, start := call(t, http.MethodPost, e.a.url+"/api/device/pair", "", map[string]string{keyName: "desk\x01top",
			keyHostname: hostName, "os": osLinux, "arch": archAMD64})
		if status != http.StatusOK || start["expires_in"] != float64(60) || start["interval"] != float64(1) {
			t.Fatalf("pair = %d %v", status, start)
		}
		code := str(t, start, keyUserCode)
		if len(code) != 9 || code[4] != '-' {
			t.Fatalf("user code %q", code)
		}
		poll := func() (int, map[string]any) {
			return call(t, http.MethodPost, e.b.url+"/api/device/pair/token", "", map[string]string{keyDeviceCode: str(t, start, keyDeviceCode)})
		}
		out := approvePairing(t, e, poll, code)
		finishPairing(t, e, poll, out)
	})
}

func approvePairing(t *testing.T, e *env, poll func() (int, map[string]any), code string) map[string]any {
	t.Helper()
	st := e.st
	if s, out := poll(); s != http.StatusBadRequest || out["error"] != hub.PairPending {
		t.Fatalf("first poll = %d %v", s, out)
	}
	if s, out := poll(); s != http.StatusBadRequest || out["error"] != hub.PairSlowDown {
		t.Fatalf("fast poll = %d %v", s, out)
	}
	p, ok, err := st.PendingPairing(context.Background(), strings.ToLower(strings.ReplaceAll(code, "-", "")))
	if err != nil || !ok || p.Name != hostName || p.Hostname != hostName || p.Addr != "127.0.0.1" {
		t.Fatalf("pending = %+v %v %v", p, ok, err)
	}
	if _, err := st.ApprovePairing(context.Background(), code, userAlice, backendID, deviceName); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	s, out := poll()
	token, _ := out[keyToken].(string)
	if s != http.StatusOK || out["device_name"] != deviceName || out["backend"] != backendID ||
		!strings.HasPrefix(token, "hdt_") || len(token) != 47 {
		t.Fatalf("approved poll = %d %v", s, out)
	}
	return out
}

func finishPairing(t *testing.T, e *env, poll func() (int, map[string]any), out map[string]any) {
	t.Helper()
	time.Sleep(time.Second)
	if s, again := poll(); s != http.StatusBadRequest || again["error"] != hub.PairExpired {
		t.Fatalf("poll after token = %d %v", s, again)
	}
	tok := str(t, out, keyToken)
	if s, who := call(t, http.MethodGet, e.a.url+"/api/device/whoami", tok, nil); s != http.StatusOK ||
		who[keyDeviceID] != out[keyDeviceID] || who["user"] != userAlice || who["placeholder"] != placeholder ||
		who["hermes_version"] != "v0.23.1" {
		t.Fatalf("whoami = %d %v", s, who)
	}
	if s, _ := call(t, http.MethodGet, e.a.url+"/api/device/whoami", tok+"x", nil); s != http.StatusUnauthorized {
		t.Fatalf("bad token = %d", s)
	}
	ds, _ := e.st.Devices(context.Background(), userAlice)
	if len(ds) != 1 || ds[0].LastAddr != "127.0.0.1" || ds[0].LastSeenAt.IsZero() {
		t.Fatalf("devices = %+v", ds)
	}
}

func TestPairingDeclined(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		_, start := call(t, http.MethodPost, e.a.url+"/api/device/pair", "", map[string]string{keyHostname: "laptop"})
		if err := st.DenyPairing(context.Background(), str(t, start, keyUserCode)); err != nil {
			t.Fatal(err)
		}
		s, out := call(t, http.MethodPost, e.a.url+"/api/device/pair/token", "", map[string]string{keyDeviceCode: str(t, start, keyDeviceCode)})
		if s != http.StatusBadRequest || out["error"] != hub.PairDenied {
			t.Fatalf("poll = %d %v", s, out)
		}
	})
}

func TestExecLegCarriesAssertion(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		c, pc := e.connectExec(e.a, tok)
		cl := pc.Claims
		if cl.Sub != userAlice || cl.DeviceID != id || cl.DeviceName != deviceName || cl.Aud != backendID ||
			cl.Leg != assertion.LegExec || cl.Path != protocol.PluginExecPath || cl.Method != http.MethodGet ||
			cl.Expires-cl.IssuedAt > 30 || cl.Expires <= cl.IssuedAt {
			t.Fatalf("claims = %+v", cl)
		}
		if pc.Hello.Hostname != hostName {
			t.Fatalf("hello = %+v", pc.Hello)
		}
		if err := pc.Send(context.Background(), map[string]any{keyType: protocol.TypeExec, "id": "x1", "cmd": "pwd", keyCWD: deviceDir, "timeout": 5}); err != nil {
			t.Fatal(err)
		}
		if m := readJSON(t, c); m["type"] != protocol.TypeExec || m["id"] != "x1" {
			t.Fatalf("exec = %v", m)
		}
		writeJSON(t, c, map[string]any{keyType: protocol.TypeExit, "id": "x1", "code": 0})
		select {
		case m := <-pc.Messages:
			if !strings.Contains(string(m), `"exit"`) {
				t.Fatalf("plugin got %s", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("exit not relayed")
		}
		pc.Close(protocol.CloseReplaced, "replaced")
		if code := closeCode(t, c); code != protocol.CloseReplaced {
			t.Fatalf("close = %d", code)
		}
	})
}

func TestExecLegCloseRecorded(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		c, _, err := dial(t, e.a, "/api/device/exec", tok)
		if err != nil {
			t.Fatal(err)
		}
		writeJSON(t, c, protocol.Hello{Type: protocol.TypeHello, Version: 4, MaxVersion: 5, Roots: []string{"/home/alice"},
			Home: "/home/alice", Hostname: hostName, OS: osLinux, Arch: archAMD64, ClientVersion: "v0.4.0"})
		_, _, err = c.Read(context.Background())
		want := "hermes-device v0.4.0 speaks protocol 4 to 5; plugin " + devicetest.PluginVersion + " speaks 2 to 3; upgrade them together"
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != protocol.CloseUnsupportedVersion || ce.Reason != want {
			t.Fatalf("close = %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			d, _, err := st.Device(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			lc := d.LastClose
			if lc.Code == int(protocol.CloseUnsupportedVersion) && lc.Reason == want && lc.By == hub.CloseByBackend && !lc.At.IsZero() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("last close = %+v", lc)
			}
			time.Sleep(20 * time.Millisecond)
		}
		e.connectExec(e.a, tok)
		if d, _, _ := st.Device(context.Background(), id); !d.LastClose.At.IsZero() {
			t.Fatalf("last close after a new open = %+v", d.LastClose)
		}
	})
}

func TestLegRefusals(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		if _, resp, err := dial(t, e.a, "/api/device/exec", "hdt_nope"); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad token dial = %v %v", resp, err)
		}
		_, other, _ := ed25519.GenerateKey(rand.Reader)
		wrong, err := device.New(device.Options{Store: st, Access: e.access, Key: other,
			Backends: []config.Backend{{ID: backendID, URL: e.gw.URL(), Devices: true}},
			Dialers: map[string]device.Dialer{backendID: gateway.NewDialer(gateway.Credentials{BaseURL: e.gw.URL(),
				Provider: providerBasic, Username: hubUser, Password: "pw"})}})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(wrong.Handler())
		t.Cleanup(srv.Close)
		c, _, err := dial(t, &replica{url: srv.URL}, "/api/device/exec", tok)
		if err != nil {
			t.Fatal(err)
		}
		if code := closeCode(t, c); code != protocol.CloseAssertionRefused {
			t.Fatalf("wrong key close = %d", code)
		}
		if e.plugin.RefusedAssertions() != 1 {
			t.Fatalf("refused = %d", e.plugin.RefusedAssertions())
		}
		e.acp.HangUp()
		if _, resp, err := dial(t, e.a, "/api/device/acp", tok); err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("acp down = %v %v", resp, err)
		}
	})
}

func TestACPLeg(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		c, _, err := dial(t, e.b, "/api/device/acp", tok)
		if err != nil {
			t.Fatal(err)
		}
		msg := map[string]any{keyJSONRPC: jsonRPC, "id": 1, keyMethod: acpInitialize, "params": map[string]any{}}
		writeJSON(t, c, msg)
		if m := readJSON(t, c); m["id"] != float64(1) || m[keyResult] == nil {
			t.Fatalf("initialize = %v", m)
		}
		cl := e.acp.Claims()
		if len(cl) != 1 || cl[0].Leg != assertion.LegACP || cl[0].Path != protocol.ACPHostPath || cl[0].DeviceID != id {
			t.Fatalf("claims = %+v", cl)
		}
		e.acp.SetCloseWith(websocket.StatusInternalError)
		c2, _, err := dial(t, e.a, "/api/device/acp", tok)
		if err != nil {
			t.Fatal(err)
		}
		writeJSON(t, c2, msg)
		if code := closeCode(t, c2); code != websocket.StatusInternalError {
			t.Fatalf("relayed close = %d", code)
		}
		if cl := e.acp.Claims(); len(cl) != 2 || cl[0].Nonce == cl[1].Nonce {
			t.Fatalf("nonces = %+v", cl)
		}
	})
}

func TestRevokeClosesLegsOnEveryReplica(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		ca, _ := e.connectExec(e.a, tok)
		cb, _, err := dial(t, e.b, "/api/device/acp", tok)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := st.RevokeDevice(context.Background(), userAlice, id); !ok || err != nil {
			t.Fatalf("revoke = %v %v", ok, err)
		}
		e.a.dev.CloseDevice(id)
		if code := closeCode(t, ca); code != protocol.CloseUnauthorized {
			t.Fatalf("local close = %d", code)
		}
		if code := closeCode(t, cb); code != protocol.CloseUnauthorized {
			t.Fatalf("other replica close = %d", code)
		}
		if s, _ := call(t, http.MethodGet, e.b.url+"/api/device/whoami", tok, nil); s != http.StatusUnauthorized {
			t.Fatalf("whoami after revoke = %d", s)
		}
	})
}

func TestBackendAccessRechecked(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		c, _ := e.connectExec(e.a, tok)
		e.access.denied.Store(true)
		if code := closeCode(t, c); code != protocol.CloseUnauthorized {
			t.Fatalf("close = %d", code)
		}
		if _, resp, err := dial(t, e.a, "/api/device/exec", tok); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("dial = %v %v", resp, err)
		}
	})
}

func TestPingTimeout(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		c, _, err := dial(t, e.a, "/api/device/acp", tok)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if code := closeCode(t, c); code != protocol.ClosePingTimeout {
			t.Fatalf("close = %d", code)
		}
	})
}

func TestBindAPI(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.connectExec(e.a, tok)
		u := e.b.url + "/api/device/sessions/s1/binding"
		s, out := call(t, http.MethodPut, u, tok, map[string]any{keyDir: deviceDir, keyDeviceID: "d_forged"})
		if s != http.StatusOK || out[keyDeviceID] != id || out[keyDir] != deviceDir {
			t.Fatalf("bind = %d %v", s, out)
		}
		if s, out := call(t, http.MethodPut, u, tok, map[string]any{keyDir: "/etc"}); s != http.StatusUnprocessableEntity || out["error"] != "dir_refused" {
			t.Fatalf("refused bind = %d %v", s, out)
		}
		e.plugin.SetBinding(sessionOther, "d_other", "/home/alice/x")
		if s, out := call(t, http.MethodDelete, e.b.url+"/api/device/sessions/s-other/binding", tok, nil); s != http.StatusForbidden || out["error"] != "forbidden" {
			t.Fatalf("unbind other = %d %v", s, out)
		}
		e.plugin.SetParent("s1-child", "s1")
		if s, _ := call(t, http.MethodDelete, e.b.url+"/api/device/sessions/s1-child/binding", tok, nil); s != http.StatusForbidden {
			t.Fatalf("unbind inherited = %d", s)
		}
		if s, _ := call(t, http.MethodDelete, u, tok, nil); s != http.StatusNoContent {
			t.Fatalf("unbind = %d", s)
		}
		if _, ok := e.plugin.Bindings()["s1"]; ok {
			t.Fatal("binding still there")
		}
		if s, _ := call(t, http.MethodPut, u, "", map[string]any{keyDir: deviceDir}); s != http.StatusUnauthorized {
			t.Fatalf("no token = %d", s)
		}
	})
}

// gwConn is a device's gateway leg.
type gwConn struct {
	t *testing.T
	c *websocket.Conn
}

func (e *env) gatewayLeg(r *replica, token string) *gwConn {
	t := e.t
	t.Helper()
	c, _, err := dial(t, r, "/api/device/gateway", token)
	if err != nil {
		t.Fatal(err)
	}
	ready := readJSON(t, c)
	if params, _ := ready["params"].(map[string]any); params["type"] != gateway.ReadyEvent {
		t.Fatalf("first message = %v", ready)
	}
	return &gwConn{t: t, c: c}
}

// request sends one request and returns its answer, skipping events.
func (g *gwConn) request(id any, method string, params any) map[string]any {
	g.t.Helper()
	msg := map[string]any{keyJSONRPC: jsonRPC, "id": id, keyMethod: method}
	if params != nil {
		msg["params"] = params
	}
	writeJSON(g.t, g.c, msg)
	for {
		m := readJSON(g.t, g.c)
		if m["method"] == nil {
			return m
		}
	}
}

func reason(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	d, _ := e["data"].(map[string]any)
	r, _ := d["reason"].(string)
	return r
}

func (e *env) called(method string) []gatewaytest.Call {
	var out []gatewaytest.Call
	for _, c := range e.gw.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func TestGatewayLegDenyList(t *testing.T) {
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
			{methodShell, map[string]any{keyCommand: "id"}},
			{"vault.list", map[string]any{}},
			{methodConfigSet, map[string]any{keyKey: "skin", keyValue: "mono"}},
			{methodConfigSet, map[string]any{keyKey: "personality", keyValue: "pirate"}},
			{methodConfigSet, map[string]any{keyKey: keyReasoning, keyValue: "high", keyScope: "global"}},
			{methodConfigSet, map[string]any{keyKey: keyReasoning, keyValue: "show"}},
			{methodConfigSet, map[string]any{keyKey: "yolo", keyValue: "1", keyScope: "global"}},
			{methodConfigSet, map[string]any{keyKey: "details_mode", keyValue: "expanded"}},
			{methodMutate, map[string]any{keySessionID: "s1", keyOperation: "editor_mcp", keyPayload: map[string]any{}}},
			{methodFileAttach, map[string]any{keyPath: "/etc/passwd"}},
			{"prompt.background", map[string]any{keySessionID: "s1", keyText: "x"}},
			{"session.prune", map[string]any{"filters": map[string]any{}}},
			{"bot_relay.deliver", map[string]any{}},
			{"cron.cancel", map[string]any{}},
			{"groups.create", map[string]any{}},
			{"llm.oneshot", map[string]any{}},
			{methodConfigSet, map[string]any{keySessionID: "s1", keyKey: keyReasoning, keyValue: "show\u001f"}},
			{methodSlash, map[string]any{keySessionID: "s1", keyCommand: "/skin mono"}},
			{methodSlash, map[string]any{keySessionID: "s1", keyCommand: "/reasoning high --global"}},
			{methodSlash, map[string]any{keySessionID: "s1", keyCommand: "/reasoning show"}},
			{methodSlash, map[string]any{keySessionID: "s1", keyCommand: "/busy queue"}},
			{"command.dispatch", map[string]any{keySessionID: "s1", keyName: "personality", "arg": "pirate"}},
			{methodList, map[string]any{keyProfile: nameOther}},
			{"session.foo", map[string]any{keySessionID: "s1"}},
			{"session.events.since", map[string]any{keySessionID: "s1"}},
			{methodTitle, map[string]any{keySessionID: "s1", keyTitle: "x"}},
			{"session.save", map[string]any{keySessionID: "s1"}},
			{"paste.collapse", map[string]any{keyText: "x"}},
			{"billing.charge", map[string]any{"amount_usd": 5}},
			{methodMutate, map[string]any{keySessionID: "s1", keyOperation: "import", keyPayload: map[string]any{}}},
			{methodMutate, map[string]any{keySessionID: "s1", keyPayload: map[string]any{}}},
		} {
			if m := g.request("r-"+tc.method, tc.method, tc.params); reason(m) != reasonDenied {
				t.Fatalf("%s %v = %v", tc.method, tc.params, m)
			}
		}
		if got := e.called(methodConfigSet); len(got) != 0 {
			t.Fatalf("denied calls reached the gateway: %v", got)
		}
		for method, why := range map[string]string{"session.foo": "the hub allows only the methods", methodShell: "runs programs in the Hermes pod",
			methodTitle: "refuses it on the hub's connection"} {
			m := g.request("why-"+method, method, map[string]any{})
			rpcErr, _ := m["error"].(map[string]any)
			if msg, _ := rpcErr["message"].(string); !strings.HasPrefix(msg, method+" is not available through talaria: ") || !strings.Contains(msg, why) {
				t.Fatalf("%s message = %v", method, m)
			}
		}
		if m := g.request("resize", "terminal.resize", map[string]any{keySessionID: "s1", keyCols: 80}); reason(m) == reasonDenied {
			t.Fatalf("terminal.resize = %v", m)
		}
		if got := e.called("terminal.resize"); len(got) != 1 {
			t.Fatalf("terminal.resize calls = %v", got)
		}
		m := g.request(7, "complete.path", map[string]any{"word": "/"})
		cres, resOK := m[keyResult].(map[string]any)
		items, itemsOK := cres["items"].([]any)
		if m["id"] != float64(7) || !resOK || !itemsOK || len(items) != 0 {
			t.Fatalf("complete.path = %v", m)
		}
		for _, p := range []map[string]any{
			{keySessionID: "s1", keyKey: "verbose", keyValue: "all"},
			{keySessionID: "s1", keyKey: keyReasoning, keyValue: "high"},
			{keySessionID: "s1", keyKey: "yolo", keyValue: "1"},
			{keySessionID: "s1", keyKey: "fast", keyValue: "fast"},
		} {
			g.request("ok-"+str(t, p, keyKey), methodConfigSet, p)
		}
		if got := e.called(methodConfigSet); len(got) != 4 {
			t.Fatalf("session config.set calls = %v", got)
		}
		if m := g.request("hermes-hub-1", methodList, map[string]any{}); m["error"] == nil {
			t.Fatalf("own id = %v", m)
		}
		writeJSON(t, g.c, []any{map[string]any{keyJSONRPC: jsonRPC, "id": 1, keyMethod: methodShell}})
		if m := readJSON(t, g.c); m["error"] == nil {
			t.Fatalf("batch = %v", m)
		}
		if m := g.request("dup", methodList, map[string]any{keyScope: scopeStored}); m["error"] != nil {
			t.Fatalf("session.list = %v", m)
		}
		if m := g.request(1.0, methodList, map[string]any{keyScope: scopeStored}); m["id"] != 1.0 || m["error"] != nil {
			t.Fatalf("numeric id = %v", m)
		}
		g.request("slash", methodSlash, map[string]any{keySessionID: "s1", keyCommand: "/title new name"})
		if got := e.called(methodSlash); len(got) != 1 {
			t.Fatalf("allowed slash.exec calls = %v", got)
		}
	})
}

func TestGatewayLegCreate(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.connectExec(e.a, tok)
		g := e.gatewayLeg(e.b, tok)
		m := g.request("r1", "session.create", map[string]any{keyRequestID: "q1", keySource: sourceTUI, keyCWD: deviceDir,
			"editor": map[string]any{"mcp_servers": []any{"x"}, keyName: "vim"}})
		res, _ := m[keyResult].(map[string]any)
		sid, _ := res[keySessionID].(string)
		if m["id"] != "r1" || sid == "" {
			t.Fatalf("create = %v", m)
		}
		s, _ := e.gw.Session(sid)
		if s.CWD != placeholder || s.RowCWD != deviceDir {
			t.Fatalf("session cwd %q row %q", s.CWD, s.RowCWD)
		}
		created := e.called("session.create")
		if ed, _ := created[0].Params["editor"].(map[string]any); ed["mcp_servers"] != nil || ed[keyName] != "vim" {
			t.Fatalf("editor = %v", created[0].Params["editor"])
		}
		if b := e.plugin.Bindings()[sid]; b.DeviceID != id || b.Dir != deviceDir {
			t.Fatalf("binding = %+v", b)
		}
		binds := e.plugin.Binds()
		again := g.request("r2", "session.create", map[string]any{keyRequestID: "q1", keySource: sourceTUI,
			keyCWD: placeholder, "row_cwd": deviceDir})
		againRes, againOK := again[keyResult].(map[string]any)
		if !againOK || againRes[keySessionID] != sid || e.plugin.Binds() != binds {
			t.Fatalf("retry = %v, binds %d -> %d", again, binds, e.plugin.Binds())
		}
		if m := g.request("r3", "session.create", map[string]any{keyRequestID: "q3", keyCWD: "relative"}); reason(m) != "hermes_device_cwd" {
			t.Fatalf("relative = %v", m)
		}
		if m := g.request("r4", "session.create", map[string]any{keyRequestID: "q4", keyCWD: deviceDir, "safe_mode": true}); reason(m) != "hermes_device_bypass" {
			t.Fatalf("safe mode = %v", m)
		}
		before := len(e.gw.SessionIDs())
		m = g.request("r5", "session.create", map[string]any{keyRequestID: "q5", keyCWD: "/etc"})
		er, _ := m["error"].(map[string]any)
		data, _ := er["data"].(map[string]any)
		if reason(m) != "hermes_device_bind" || data["hub_error"] != "dir_refused" {
			t.Fatalf("refused bind = %v", m)
		}
		if after := len(e.gw.SessionIDs()); after != before {
			t.Fatalf("unbound session kept: %d -> %d", before, after)
		}
		if m := g.request("r6", "session.resume", map[string]any{keySessionID: sid}); m["error"] != nil {
			t.Fatalf("resume own = %v", m)
		}
		g.request("r7", methodMutate, map[string]any{keySessionID: sid, keyRequestID: "w1", keyExpectedRev: 0,
			keyOperation: opWorkspace, keyPayload: map[string]any{keyCWD: otherDir}})
		if b := e.plugin.Bindings()[sid]; b.Dir != otherDir {
			t.Fatalf("workspace binding = %+v", b)
		}
		if s, _ := e.gw.Session(sid); s.CWD != placeholder {
			t.Fatalf("workspace moved the session to %q", s.CWD)
		}
		if m := g.request("r8", methodMutate, map[string]any{keySessionID: sid, keyRequestID: "w2", keyExpectedRev: 99,
			keyOperation: opWorkspace, keyPayload: map[string]any{keyCWD: "/home/alice/third"}}); m["error"] == nil {
			t.Fatalf("stale move = %v", m)
		}
		deadline := time.Now().Add(5 * time.Second)
		for e.plugin.Bindings()[sid].Dir != otherDir {
			if time.Now().After(deadline) {
				t.Fatalf("binding after refused move = %+v", e.plugin.Bindings()[sid])
			}
			time.Sleep(20 * time.Millisecond)
		}
		for _, c := range e.called(methodMutate) {
			if p, _ := c.Params["payload"].(map[string]any); c.Params[keyOperation] == opWorkspace && p[keyCWD] != placeholder {
				t.Fatalf("workspace cwd sent = %v", p)
			}
		}
	})
}

func TestGatewayLegOtherDevice(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		e.gw.AddSession(&gatewaytest.Session{ID: sessionOther, Title: titleOther, Live: true, StartedAt: 1})
		e.gw.AddSession(&gatewaytest.Session{ID: sessionPod, Live: true, StartedAt: 2})
		e.plugin.SetBinding(sessionOther, "d_laptop", "/home/alice/x")
		e.plugin.SetParent("s-other-child", sessionOther)
		g := e.gatewayLeg(e.a, tok)
		if m := g.request(1, "session.resume", map[string]any{keySessionID: sessionOther}); reason(m) != reasonOtherDevice {
			t.Fatalf("resume other = %v", m)
		}
		if m := g.request(2, "prompt.submit", map[string]any{keySessionID: "s-other-child", "text": "hi", keySubmission: "x"}); reason(m) != reasonOtherDevice {
			t.Fatalf("submit other = %v", m)
		}
		if m := g.request(3, "session.resume", map[string]any{keySessionID: sessionPod}); reason(m) != reasonNotBound {
			t.Fatalf("resume pod session = %v", m)
		}
		if got := len(e.called("session.resume")) + len(e.called("prompt.submit")); got != 0 {
			t.Fatalf("%d refused calls reached the gateway", got)
		}
		for i, p := range []map[string]any{
			{keySessionID: "", keyTitle: titleOther}, {keyTitle: titleOther}, {keySessionID: sessionOther, keyTitle: titleOther},
			{keySessionID: "", "latest": sourceTUI}, {"latest": sourceTUI}, {keySessionID: ""}, {},
		} {
			if m := g.request(40+i, "session.resume", p); reason(m) != reasonDenied {
				t.Fatalf("resume %v = %v", p, m)
			}
		}
		if got := e.called("session.resume"); len(got) != 0 {
			t.Fatalf("resumes by title or latest reached the gateway: %v", got)
		}
		if m := g.request(6, methodMutate, map[string]any{keySessionID: sessionPod, "request_id": "a", keyExpectedRev: 0,
			"operation": "archive", "payload": map[string]any{"archived": true}}); reason(m) != reasonNotBound {
			t.Fatalf("archive pod session = %v", m)
		}
	})
}

const (
	keyCommand        = "command"
	keyType           = "type"
	methodSlash       = "slash.exec"
	methodShell       = "shell.exec"
	methodMutate      = "session.mutate"
	methodTitle       = "session.title"
	reasonDenied      = "hermes_device_denied"
	sourceTUI         = "tui"
	keyExpectedRev    = "expected_revision"
	sessionPod        = "s-pod"
	reasonOtherDevice = "hermes_device_other_device"
	reasonNotBound    = "hermes_device_not_bound"
	sandboxID         = "sandbox-alice"
	sandboxName       = "Alice's sandbox"
	sandboxToken      = "hdt_sandbox-token-for-tests-only-0123456789" //nolint:gosec // a test token
)

func TestServiceDevice(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		c, pc := e.connectExec(e.a, sandboxToken)
		if pc.Claims.DeviceID != sandboxID || pc.Claims.Sub != hub.ServiceUser || pc.Claims.DeviceName != sandboxName {
			t.Fatalf("claims = %+v", pc.Claims)
		}
		if s, who := call(t, http.MethodGet, e.a.url+"/api/device/whoami", sandboxToken, nil); s != http.StatusOK || who["device_id"] != sandboxID {
			t.Fatalf("whoami = %d %v", s, who)
		}
		if s, out := call(t, http.MethodPut, e.a.url+"/api/device/sessions/s1/binding", sandboxToken, map[string]any{"dir": deviceDir}); s != http.StatusForbidden || out["error"] != "forbidden" {
			t.Fatalf("bind = %d %v", s, out)
		}
		if s, _ := call(t, http.MethodDelete, e.a.url+"/api/device/sessions/s1/binding", sandboxToken, nil); s != http.StatusForbidden {
			t.Fatalf("unbind = %d", s)
		}
		for _, leg := range []string{"/api/device/gateway", "/api/device/acp"} {
			if _, resp, err := dial(t, e.b, leg, sandboxToken); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s = %v %v", leg, resp, err)
			}
		}
		if e.plugin.Binds() != 0 {
			t.Fatal("a sandbox bind reached the plugin")
		}
		sbs := e.b.dev.Sandboxes(context.Background(), userAlice)
		if len(sbs) != 1 || sbs[0].Online == nil || !*sbs[0].Online || sbs[0].BackendID != backendID {
			t.Fatalf("sandboxes = %+v", sbs)
		}
		if sbs := e.b.dev.Sandboxes(context.Background(), "bob"); len(sbs) != 0 {
			t.Fatalf("bob sees %+v", sbs)
		}
		for i := range 4 {
			time.Sleep(100 * time.Millisecond)
			writeJSON(t, c, map[string]any{keyType: protocol.TypePing, "seq": i})
			if m := readJSON(t, c); m["type"] != protocol.TypePong {
				t.Fatalf("sandbox leg after rechecks = %v", m)
			}
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
		deadline := time.Now().Add(5 * time.Second)
		for {
			sbs = e.b.dev.Sandboxes(context.Background(), userAlice)
			if sbs[0].Online != nil && !*sbs[0].Online {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("sandbox still online")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func TestGatewayLegDrop(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		g := e.gatewayLeg(e.a, tok)
		e.gw.DropConnections()
		if code := closeCode(t, g.c); code != protocol.CloseBackendUnavailable {
			t.Fatalf("close = %d", code)
		}
	})
}

func TestStoreErrorsAreNotUnauthorized(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t, st)
	tok, _ := e.pair(e.a)
	_ = st.Close()
	s, out := call(t, http.MethodGet, e.a.url+"/api/device/whoami", tok, nil)
	if s == http.StatusUnauthorized || s < 500 {
		t.Fatalf("whoami with a broken store = %d %v", s, out)
	}
}

func TestGatewayLegListOwnSessions(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		for _, sid := range []string{sessionMine, sessionOther, sessionUnbound} {
			e.gw.AddSession(&gatewaytest.Session{ID: sid, Live: true, Title: sid})
		}
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		e.plugin.SetBinding(sessionOther, otherDevice, "/home/bob")
		g := e.gatewayLeg(e.b, tok)
		for _, scope := range []string{"live", scopeStored} {
			m := g.request("list-"+scope, methodList, map[string]any{keyScope: scope})
			res, _ := m[keyResult].(map[string]any)
			rows, _ := res["sessions"].([]any)
			if len(rows) != 1 {
				t.Fatalf("%s list = %v", scope, m)
			}
			if row, _ := rows[0].(map[string]any); row[keySessionID] != sessionMine {
				t.Fatalf("%s list = %v", scope, m)
			}
		}
	})
}

func TestGatewayLegImageRender(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		render := map[string]any{keyPath: "/opt/data/images/a.png", keyCols: 40}
		g := e.gatewayLeg(e.b, tok)
		if m := g.request("plain", "image.render", render); reason(m) != reasonDenied {
			t.Fatalf("image.render without confinement = %v", m)
		}
		e.gw.SetCapabilities("image-render-confined-v1")
		g = e.gatewayLeg(e.b, tok)
		g.request("confined", "image.render", render)
		if got := e.called("image.render"); len(got) != 1 {
			t.Fatalf("forwarded image.render calls = %v", got)
		}
		if m := g.request("extra", "image.render", map[string]any{keyPath: "/x.png", "profile_home": "/"}); reason(m) != reasonDenied {
			t.Fatalf("image.render with another param = %v", m)
		}
		if got := e.called("image.render"); len(got) != 1 {
			t.Fatalf("forwarded image.render calls = %v", got)
		}
	})
}

const capServerRequests = "server_requests"

func TestGatewayLegClientCapabilities(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, _ := e.pair(e.a)
		g := e.gatewayLeg(e.b, tok)
		g.request("ok", "client.capabilities", map[string]any{capServerRequests: true})
		got := e.called("client.capabilities")
		if len(got) != 1 || len(got[0].Params) != 1 || got[0].Params[capServerRequests] != true {
			t.Fatalf("forwarded client.capabilities calls = %v", got)
		}
		for name, params := range map[string]map[string]any{
			"local":   {"desktop_local": true},
			"both":    {capServerRequests: true, "desktop_local": true},
			"string":  {capServerRequests: "yes"},
			"null":    {capServerRequests: nil},
			"nothing": {},
		} {
			if m := g.request(name, "client.capabilities", params); reason(m) != reasonDenied {
				t.Fatalf("client.capabilities %s = %v", name, m)
			}
		}
		if got := e.called("client.capabilities"); len(got) != 1 {
			t.Fatalf("forwarded client.capabilities calls = %v", got)
		}
	})
}

func TestGatewayLegListAudience(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		for _, sid := range []string{sessionMine, sessionOther} {
			e.gw.AddSession(&gatewaytest.Session{ID: sid, Live: true, Title: sid})
		}
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		e.plugin.SetBinding(sessionOther, otherDevice, "/home/bob")
		e.gw.SetCapabilities("session-list-audience-v1")
		g := e.gatewayLeg(e.b, tok)
		if m := g.request("unjudged", methodList, map[string]any{keyPicker: true}); reason(m) != reasonDenied {
			t.Fatalf("picker with no plugin judging the audience = %v", m)
		}
		e.gw.SetAudience("hermes-device:"+id, sessionMine)
		g = e.gatewayLeg(e.b, tok)
		m := g.request("scoped", methodList, map[string]any{keyPicker: true, "audience": "hermes-device:" + otherDevice})
		res, _ := m[keyResult].(map[string]any)
		if rows, _ := res["sessions"].([]any); len(rows) != 1 {
			t.Fatalf("scoped list = %v", m)
		}
		var last gatewaytest.Call
		for _, c := range e.called(methodList) {
			last = c
		}
		if last.Params["audience"] != "hermes-device:"+id {
			t.Fatalf("forwarded params = %v", last.Params)
		}
	})
}

const keyChoiceT = "choice"

func TestGatewayLegRecordsApprovalAnswers(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		e.gw.AddSession(&gatewaytest.Session{ID: sessionMine, Live: true, Title: sessionMine})
		e.plugin.SetBinding(sessionMine, id, deviceDir)
		g := e.gatewayLeg(e.b, tok)
		ctx := context.Background()
		cmd := "rm -rf build"
		e.gw.Ask(sessionMine, gateway.Prompt{Kind: gateway.PromptApproval, PromptID: "ap-9", Command: &cmd,
			Choices: []string{hub.ChoiceOnce, hub.ChoiceDeny, hub.ChoiceAlways}})
		call := func(rid, method string, params map[string]any) map[string]any {
			writeJSON(t, g.c, map[string]any{keyJSONRPC: jsonRPC, "id": rid, keyMethod: method, keyParams: params})
			for {
				if m := readJSON(t, g.c); m["id"] == rid && m[keyMethod] == nil {
					return m
				}
			}
		}
		respond := func(rid, prompt, choice string) map[string]any {
			return call(rid, "approval.respond", map[string]any{keySessionID: sessionMine, "execution_generation": 0,
				"prompt_id": prompt, keyChoiceT: choice})
		}
		if m := respond("r1", "ap-9", hub.ChoiceAlways); m["error"] != nil {
			t.Fatalf("respond = %v", m)
		}
		want := hub.PromptAnswer{Choice: hub.ChoiceAlways, Via: hub.AnsweredOnDevice, Device: deviceName}
		if a, ok, err := st.PromptAnswer(ctx, backendID, "ap-9"); a != want || !ok || err != nil {
			t.Fatalf("answer = %+v %v %v", a, ok, err)
		}
		respond("r2", "ap-gone", hub.ChoiceOnce)
		if _, ok, _ := st.PromptAnswer(ctx, backendID, "ap-gone"); ok {
			t.Fatal("kept the answer to an approval the gateway did not have")
		}
		respond("r3", "ap-9", hub.ChoiceOnce)
		if a, ok, _ := st.PromptAnswer(ctx, backendID, "ap-9"); a != want || !ok {
			t.Fatalf("a late answer replaced the first: %+v %v", a, ok)
		}
		writeJSON(t, g.c, map[string]any{keyJSONRPC: jsonRPC, keyMethod: "approval.respond", keyParams: map[string]any{
			keySessionID: sessionMine, "execution_generation": 0, "prompt_id": "ap-11", keyChoiceT: hub.ChoiceAlways}})
		call("fence0", methodList, map[string]any{})
		if _, ok, _ := st.PromptAnswer(ctx, backendID, "ap-11"); ok {
			t.Fatal("recorded the answer of a notification")
		}
		e.gw.ServerRequest("srq-7", "approval", map[string]any{keySessionID: "live-1", "request_id": "ap-10", keyCommand: cmd})
		for {
			if m := readJSON(t, g.c); m["id"] == "srq-7" {
				break
			}
		}
		writeJSON(t, g.c, map[string]any{keyJSONRPC: jsonRPC, "id": "srq-7", keyResult: map[string]any{keyChoiceT: hub.ChoiceOnce}})
		call("fence", methodList, map[string]any{})
		want.Choice = hub.ChoiceOnce
		if a, ok, _ := st.PromptAnswer(ctx, backendID, "ap-10"); a != want || !ok {
			t.Fatalf("server request answer = %+v %v", a, ok)
		}
	})
}
