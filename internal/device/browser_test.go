package device_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

const (
	userAlice  = "alice"
	idSlow     = "in-flight"
	opReadText = "read_text"
	opSnapshot = "snapshot"
	idKill     = "kill"
	opNavigate = "navigate"
	idOff      = "off"
)

type fakeBrowser struct {
	mu      sync.Mutex
	up      bool
	token   string
	device  string
	auth    []broker.AuthStatus
	changed chan struct{}
}

func newFakeBrowser(up bool) *fakeBrowser { return &fakeBrowser{up: up, changed: make(chan struct{})} }

func (f *fakeBrowser) set(up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = up
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeBrowser) setToken(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = tok
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeBrowser) Token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

func (f *fakeBrowser) SetDevice(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.device = id + " " + name
}

func (f *fakeBrowser) SetAuth(a broker.AuthStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, a)
}

func (f *fakeBrowser) lastAuth() broker.AuthStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auth) == 0 {
		return broker.AuthStatus{}
	}
	return f.auth[len(f.auth)-1]
}

// waitStatus reads until a browser_status that ok accepts.
func waitStatus(ctx context.Context, t *testing.T, conn *websocket.Conn, ok func(protocol.BrowserStatus) bool) protocol.BrowserStatus {
	t.Helper()
	for {
		var st protocol.BrowserStatus
		if !readType(ctx, t, conn, protocol.TypeBrowserStatus, &st) || ok(st) {
			return st
		}
	}
}

// signIn plays the hub: it waits for the device's browser_auth and accepts
// it.
func signIn(ctx context.Context, t *testing.T, conn *websocket.Conn) {
	t.Helper()
	var a protocol.BrowserAuth
	if !readType(ctx, t, conn, protocol.TypeBrowserAuth, &a) {
		t.Error("no browser_auth")
		return
	}
	writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, OK: true, User: userAlice})
}

func (f *fakeBrowser) Connected() (broker.Hello, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return broker.Hello{}, false
	}
	return broker.Hello{Version: "0.2.0", Browser: "Firefox 157.0.1"}, true
}

func (f *fakeBrowser) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (*fakeBrowser) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	switch op {
	case opReadText:
		raw, _ := args.(json.RawMessage)
		return json.RawMessage(`{"title":"Example Domain","args":` + string(raw) + `}`), nil
	case "tabs_list":
		return nil, broker.ErrNotConnected
	case "back":
		return nil, broker.ErrClosed
	case opNavigate:
		return nil, &broker.Error{Op: opNavigate, Code: broker.CodeRefused, Message: "tab 1 is not in the Hermes tab group"}
	case "screenshot":
		return json.RawMessage(`"` + strings.Repeat("a", protocol.MaxBrowserResult) + `"`), nil
	case opSnapshot:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("boom")
}

func readType(ctx context.Context, t *testing.T, conn *websocket.Conn, typ string, v any) bool {
	t.Helper()
	for {
		got, data, err := readMsg(ctx, conn)
		if err != nil {
			return false
		}
		if got == typ {
			if err := json.Unmarshal(data, v); err != nil {
				t.Error(err)
				return false
			}
			return true
		}
	}
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

func TestBrowserHelloOffersCapability(t *testing.T) {
	t.Parallel()
	hellos := make(chan protocol.Hello, 2)
	f := &fakeHub{handler: func(ctx context.Context, _ *websocket.Conn, h protocol.Hello) {
		hellos <- h
		<-ctx.Done()
	}}
	srv := serve(t, f)
	opts := options(t, srv.URL, t.TempDir())
	start(t, opts)
	if h := recv(t, hellos); len(h.Capabilities) != 0 {
		t.Errorf("capabilities without a browser = %v", h.Capabilities)
	}
	srv2 := serve(t, f)
	opts = options(t, srv2.URL, t.TempDir())
	opts.Browser = newFakeBrowser(false)
	start(t, opts)
	if h := recv(t, hellos); len(h.Capabilities) != 1 || h.Capabilities[0] != protocol.CapabilityBrowser {
		t.Errorf("capabilities = %v", h.Capabilities)
	}
}

func TestBrowserStatus(t *testing.T) {
	t.Parallel()
	fb := newFakeBrowser(false)
	got := make(chan protocol.BrowserStatus, 4)
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		for {
			var st protocol.BrowserStatus
			if !readType(ctx, t, conn, protocol.TypeBrowserStatus, &st) {
				return
			}
			got <- st
		}
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = fb
	start(t, opts)
	if st := recv(t, got); st.Connected {
		t.Fatalf("first status = %+v", st)
	}
	fb.set(true)
	if st := recv(t, got); !st.Connected || st.Browser != "Firefox 157.0.1" || st.ExtensionVersion != "0.2.0" {
		t.Fatalf("status after connect = %+v", st)
	}
	fb.set(false)
	if st := recv(t, got); st.Connected {
		t.Fatalf("status after disconnect = %+v", st)
	}
}

func TestBrowserRequests(t *testing.T) {
	t.Parallel()
	results := make(chan map[string]protocol.BrowserResult, 1)
	reqs := []protocol.Browser{
		{ID: "ok", Op: opReadText, Args: json.RawMessage(`{"tab_id":2}`)},
		{ID: "bad", Op: "eval_js"},
		{ID: idOff, Op: "tabs_list"},
		{ID: "closed", Op: "back"},
		{ID: "scope", Op: opNavigate},
		{ID: "err", Op: "click"},
		{ID: "big", Op: "screenshot"},
		{ID: "slow", Op: opSnapshot, Timeout: 0.05},
		{ID: idKill, Op: opSnapshot, Timeout: 30},
	}
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		signIn(ctx, t, conn)
		for _, r := range reqs {
			r.Type = protocol.TypeBrowser
			writeJSON(ctx, conn, r)
		}
		writeJSON(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: idKill})
		got := map[string]protocol.BrowserResult{}
		for len(got) < len(reqs) {
			var r protocol.BrowserResult
			if !readType(ctx, t, conn, protocol.TypeBrowserResult, &r) {
				return
			}
			got[r.ID] = r
		}
		results <- got
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = newFakeBrowser(true)
	start(t, opts)
	got := recv(t, results)
	want := map[string]string{
		"bad": protocol.BrowserRefused, idOff: protocol.BrowserNotConnected, "closed": protocol.BrowserNotConnected,
		"scope": protocol.BrowserRefused, "err": protocol.BrowserFailed, "big": protocol.BrowserFailed,
		"slow": protocol.BrowserTimeout, idKill: protocol.BrowserCancelled,
	}
	for id, code := range want {
		if got[id].OK || got[id].Code != code {
			t.Errorf("%s: %+v, want code %s", id, got[id], code)
		}
	}
	if r := got["ok"]; !r.OK || string(r.Result) != `{"title":"Example Domain","args":{"tab_id":2}}` {
		t.Errorf("ok: %+v (%s)", r, r.Result)
	}
}

// A plugin that did not accept the capability gets no browser_status, and
// browser requests are ignored as unknown messages.
func TestBrowserWithoutCapability(t *testing.T) {
	t.Parallel()
	got := make(chan string, 8)
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		writeJSON(ctx, conn, protocol.Browser{Type: protocol.TypeBrowser, ID: "x", Op: opReadText})
		writeJSON(ctx, conn, protocol.Ping{Type: protocol.TypePing, Seq: 7})
		for {
			typ, _, err := readMsg(ctx, conn)
			if err != nil {
				return
			}
			got <- typ
			if typ == protocol.TypePong {
				close(got)
				<-ctx.Done()
				return
			}
		}
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = newFakeBrowser(true)
	start(t, opts)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case typ, ok := <-got:
			if !ok {
				return
			}
			if typ == protocol.TypeBrowserResult || typ == protocol.TypeBrowserStatus {
				t.Fatalf("got %s on a leg without the browser capability", typ)
			}
		case <-timeout:
			t.Fatal("no pong")
		}
	}
}

// The device refuses every browser request until the hub accepted the
// extension's credential, presents a new credential when the extension
// sends one, and reports the hub's answer in browser_status and to the
// extension.
func TestBrowserSignIn(t *testing.T) {
	t.Parallel()
	fb := newFakeBrowser(true)
	type step struct {
		auth   protocol.BrowserAuth
		result protocol.BrowserResult
		status protocol.BrowserStatus
	}
	steps := make(chan step, 8)
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		var st step
		readType(ctx, t, conn, protocol.TypeBrowserAuth, &st.auth)
		writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, Code: protocol.BrowserAuthSignedOut})
		writeJSON(ctx, conn, protocol.Browser{Type: protocol.TypeBrowser, ID: "a", Op: opReadText})
		readType(ctx, t, conn, protocol.TypeBrowserResult, &st.result)
		steps <- st

		st = step{}
		readType(ctx, t, conn, protocol.TypeBrowserAuth, &st.auth)
		writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, Code: protocol.BrowserAuthWrongUser,
			Error: "Firefox is signed in to Hermes as bob, not as the account this computer belongs to."})
		st.status = waitStatus(ctx, t, conn, func(b protocol.BrowserStatus) bool { return strings.Contains(b.Reason, "as bob") })
		writeJSON(ctx, conn, protocol.Browser{Type: protocol.TypeBrowser, ID: "b", Op: opReadText})
		readType(ctx, t, conn, protocol.TypeBrowserResult, &st.result)
		steps <- st

		st = step{}
		readType(ctx, t, conn, protocol.TypeBrowserAuth, &st.auth)
		writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, OK: true, User: userAlice})
		st.status = waitStatus(ctx, t, conn, func(b protocol.BrowserStatus) bool { return b.SignedIn })
		writeJSON(ctx, conn, protocol.Browser{Type: protocol.TypeBrowser, ID: "c", Op: opReadText})
		readType(ctx, t, conn, protocol.TypeBrowserResult, &st.result)
		steps <- st
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = fb
	start(t, opts)

	st := recv(t, steps)
	if st.auth.Token != "" || st.result.OK || st.result.Code != protocol.BrowserNotSignedIn ||
		!strings.Contains(st.result.Error, "Firefox is not signed in to Hermes") {
		t.Fatalf("signed out: %+v", st)
	}
	if a := fb.lastAuth(); a.State != broker.AuthSignedOut {
		t.Errorf("extension told %+v", a)
	}

	fb.setToken("hbt_bob")
	st = recv(t, steps)
	if st.auth.Token != "hbt_bob" || st.status.SignedIn || !strings.Contains(st.status.Reason, "as bob") ||
		st.result.Code != protocol.BrowserNotSignedIn || !strings.Contains(st.result.Error, "as bob") {
		t.Fatalf("wrong user: %+v", st)
	}
	if a := fb.lastAuth(); a.State != broker.AuthRefused || !strings.Contains(a.Reason, "as bob") {
		t.Errorf("extension told %+v", a)
	}

	fb.setToken("hbt_alice")
	st = recv(t, steps)
	if st.auth.Token != "hbt_alice" || !st.status.SignedIn || st.status.User != userAlice || !st.status.Connected || !st.result.OK {
		t.Fatalf("signed in: %+v", st)
	}
	if a := fb.lastAuth(); a.State != broker.AuthSignedIn || a.User != userAlice {
		t.Errorf("extension told %+v", a)
	}
}

// A browser_auth_result whose type a decoder could read two ways is
// ignored, and a sign-in the hub drops cancels the requests in flight.
func TestBrowserForgedResultAndDrop(t *testing.T) {
	t.Parallel()
	fb := newFakeBrowser(true)
	fb.setToken("hbt_alice")
	results := make(chan protocol.BrowserResult, 4)
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		var a protocol.BrowserAuth
		readType(ctx, t, conn, protocol.TypeBrowserAuth, &a)
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"browser_auth_result","type":null,"ok":true,"user":"alice"}`))
		writeJSON(ctx, conn, protocol.Browser{Type: protocol.TypeBrowser, ID: "forged", Op: opReadText})
		var r protocol.BrowserResult
		readType(ctx, t, conn, protocol.TypeBrowserResult, &r)
		results <- r
		writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, OK: true, User: userAlice})
		waitStatus(ctx, t, conn, func(b protocol.BrowserStatus) bool { return b.SignedIn })
		writeJSON(ctx, conn, protocol.Browser{Type: protocol.TypeBrowser, ID: idSlow, Op: opSnapshot, Timeout: 60})
		time.Sleep(200 * time.Millisecond)
		writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, Code: protocol.BrowserAuthInvalid,
			Error: "This Firefox sign-in is no longer valid."})
		readType(ctx, t, conn, protocol.TypeBrowserResult, &r)
		results <- r
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = fb
	start(t, opts)
	if r := recv(t, results); r.ID != "forged" || r.Code != protocol.BrowserNotSignedIn {
		t.Fatalf("request after a forged result = %+v", r)
	}
	if r := recv(t, results); r.ID != idSlow || r.Code != protocol.BrowserCancelled {
		t.Fatalf("request in flight when the sign-in was dropped = %+v", r)
	}
}

// browser_start starts Firefox through Options.StartBrowser and answers
// once the extension is connected and signed in.
func TestBrowserStart(t *testing.T) {
	t.Parallel()
	fb := newFakeBrowser(false)
	results := make(chan protocol.BrowserResult, 1)
	statuses := make(chan protocol.BrowserStatus, 1)
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		var a protocol.BrowserAuth
		// The first status and the first browser_auth come in either order.
		for got := 0; got < 2; {
			typ, data, err := readMsg(ctx, conn)
			if err != nil {
				return
			}
			switch typ {
			case protocol.TypeBrowserAuth:
				writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, Code: protocol.BrowserAuthSignedOut})
				got++
			case protocol.TypeBrowserStatus:
				var st protocol.BrowserStatus
				_ = json.Unmarshal(data, &st)
				if got < 2 {
					statuses <- st
				}
				got++
			}
		}
		writeJSON(ctx, conn, protocol.BrowserStart{Type: protocol.TypeBrowserStart, ID: "s", Timeout: 10})
		for {
			typ, data, err := readMsg(ctx, conn)
			if err != nil {
				return
			}
			switch typ {
			case protocol.TypeBrowserAuth:
				if json.Unmarshal(data, &a) == nil && a.Token == "hbt_alice" {
					writeJSON(ctx, conn, protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, OK: true, User: userAlice})
				}
			case protocol.TypeBrowserResult:
				var r protocol.BrowserResult
				_ = json.Unmarshal(data, &r)
				results <- r
				<-ctx.Done()
				return
			}
		}
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = fb
	var starts int
	opts.StartBrowser = func(context.Context) error {
		starts++
		go func() {
			fb.set(true)
			fb.setToken("hbt_alice")
		}()
		return nil
	}
	start(t, opts)
	if st := recv(t, statuses); st.Connected || !st.CanStart {
		t.Errorf("status before start = %+v", st)
	}
	r := recv(t, results)
	if !r.OK || string(r.Result) != `{"started":true,"signed_in":true,"user":"alice"}` || starts != 1 {
		t.Fatalf("start result = %+v (%s), starts = %d", r, r.Result, starts)
	}
}

// A second start within StartCooldown is refused without starting Firefox.
func TestBrowserStartCooldown(t *testing.T) {
	t.Parallel()
	results := make(chan protocol.BrowserResult, 2)
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		for _, id := range []string{"a", "b"} {
			writeJSON(ctx, conn, protocol.BrowserStart{Type: protocol.TypeBrowserStart, ID: id, Timeout: 0.3})
			var r protocol.BrowserResult
			readType(ctx, t, conn, protocol.TypeBrowserResult, &r)
			results <- r
		}
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.Browser = newFakeBrowser(false)
	var mu sync.Mutex
	starts := 0
	opts.StartBrowser = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		starts++
		return nil
	}
	start(t, opts)
	if r := recv(t, results); r.Code != protocol.BrowserTimeout {
		t.Errorf("first start = %+v", r)
	}
	if r := recv(t, results); r.Code != protocol.BrowserRefused || !strings.Contains(r.Error, "less than") {
		t.Errorf("second start = %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Errorf("Firefox started %d times", starts)
	}
}

func TestBrowserStartRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		start func(context.Context) error
		code  string
		text  string
	}{
		idOff:     {nil, protocol.BrowserRefused, "browser.start-firefox is off"},
		"fails":   {func(context.Context) error { return errors.New("no graphical session: sign in") }, protocol.BrowserFailed, "no graphical session"},
		"silence": {func(context.Context) error { return nil }, protocol.BrowserTimeout, "did not connect"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			results := make(chan protocol.BrowserResult, 1)
			f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
				writeJSON(ctx, conn, protocol.BrowserStart{Type: protocol.TypeBrowserStart, ID: "s", Timeout: 0.5})
				var r protocol.BrowserResult
				readType(ctx, t, conn, protocol.TypeBrowserResult, &r)
				results <- r
				<-ctx.Done()
			}}
			opts := options(t, serve(t, f).URL, t.TempDir())
			opts.Browser = newFakeBrowser(false)
			opts.StartBrowser = tc.start
			start(t, opts)
			r := recv(t, results)
			if r.OK || r.Code != tc.code || !strings.Contains(r.Error, tc.text) {
				t.Errorf("result = %+v, want %s with %q", r, tc.code, tc.text)
			}
		})
	}
}
