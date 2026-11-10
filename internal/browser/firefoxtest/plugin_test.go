package firefoxtest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

// dialer opens the exec leg to the relay in place of the hub.
type dialer struct{ url string }

func (d dialer) DialLeg(ctx context.Context, path string) (*websocket.Conn, error) {
	c, _, err := websocket.Dial(ctx, d.url+path, nil)
	if err == nil {
		c.SetReadLimit(64 << 20)
	}
	return c, err
}

// pluginLayer runs the real device client against the broker, with the
// plugin's Python tool layer on the other end of the exec leg:
// plugin/device/tests/test_browser_e2e.py, in the Hermes runtime. The
// relay here plays the hub and the plugin's exec route: it answers hello
// with a welcome that accepts the browser capability, then copies every
// message between the leg and the Python test as JSON lines.
func pluginLayer(ctx context.Context, t *testing.T, b *broker.Broker, formURL string) {
	if _, err := exec.LookPath("hermes"); err != nil {
		t.Skip("hermes is not on PATH; the plugin layer needs the Hermes runtime")
	}
	dir, err := os.MkdirTemp("", "hbplug")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "plugin.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()

	pyCtx, pyCancel := context.WithCancel(ctx)
	defer pyCancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relay(pyCtx, t, w, r, accepted)
	}))
	defer srv.Close()

	allow, err := allowlist.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := device.New(device.Options{
		Hub: dialer{url: "ws" + srv.URL[len("http"):]}, Allow: allow, Browser: b, ClientVersion: "e2e",
		TempDir: root, MinBackoff: 100 * time.Millisecond, MaxBackoff: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	devDone := make(chan error, 1)
	go func() { devDone <- dev.Run(pyCtx) }()
	defer func() {
		pyCancel()
		<-devDone
	}()

	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tests := filepath.Join(repo, "plugin", "device", "tests")
	py := exec.CommandContext(ctx, "sh", filepath.Join(repo, "scripts", "test-plugin.sh"), //nolint:gosec // repo script
		"-s", tests, "-t", tests, "-p", "test_browser_e2e.py", "-v")
	py.Env = append(os.Environ(), "HERMES_BROWSER_E2E_SOCKET="+sock, "HERMES_BROWSER_E2E_FORM_URL="+formURL,
		"HERMES_BROWSER_E2E_ROOT="+root)
	out, err := py.CombinedOutput()
	t.Logf("plugin layer:\n%s", out)
	if err != nil {
		t.Fatalf("plugin layer test failed: %v", err)
	}
}

// relay plays the plugin's exec route for one device connection.
func relay(ctx context.Context, t *testing.T, w http.ResponseWriter, r *http.Request, accepted <-chan net.Conn) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	defer func() { _ = ws.CloseNow() }()
	_, data, err := ws.Read(ctx)
	if err != nil {
		return
	}
	var hello protocol.Hello
	if json.Unmarshal(data, &hello) != nil || !slices.Contains(hello.Capabilities, protocol.CapabilityBrowser) {
		t.Errorf("hello without the browser capability: %s", data)
		return
	}
	for _, m := range []any{
		protocol.Welcome{Type: protocol.TypeWelcome, Version: protocol.Version, DeviceID: "d_e2e", Name: "desktop",
			PingInterval: 25, Capabilities: []string{protocol.CapabilityBrowser}},
		protocol.Resume{Type: protocol.TypeResume, Commands: []protocol.ResumeCommand{}},
	} {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Error(err)
			return
		}
		if err := ws.Write(ctx, websocket.MessageText, raw); err != nil {
			return
		}
	}
	var c net.Conn
	select {
	case c = <-accepted:
	case <-ctx.Done():
		return
	}
	defer c.Close()
	if _, err := c.Write(append(data, '\n')); err != nil {
		return
	}
	go func() {
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			if ws.Write(ctx, websocket.MessageText, sc.Bytes()) != nil {
				return
			}
		}
		_ = ws.Close(websocket.StatusNormalClosure, "plugin test done")
	}()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if _, err := c.Write(append(msg, '\n')); err != nil {
			return
		}
	}
}
