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
	opReadText = "read_text"
	opSnapshot = "snapshot"
	idKill     = "kill"
	opNavigate = "navigate"
)

type fakeBrowser struct {
	mu      sync.Mutex
	up      bool
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
		{ID: "off", Op: "tabs_list"},
		{ID: "closed", Op: "back"},
		{ID: "scope", Op: opNavigate},
		{ID: "err", Op: "click"},
		{ID: "big", Op: "screenshot"},
		{ID: "slow", Op: opSnapshot, Timeout: 0.05},
		{ID: idKill, Op: opSnapshot, Timeout: 30},
	}
	f := &fakeHub{capabilities: []string{protocol.CapabilityBrowser}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
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
		"bad": protocol.BrowserRefused, "off": protocol.BrowserNotConnected, "closed": protocol.BrowserNotConnected,
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
