// Package mcpe2e_test runs MCP servers on a device end to end: the real
// device client and MCP manager, with a fake MCP server (this test binary,
// internal/mcp/mcptest), connect over a real WebSocket to the real device
// plugin exec route, served by uvicorn in the Hermes runtime, and the
// plugin's device-mcp tools drive the server
// (plugin/device/tests/test_mcp_e2e.py). The test signs a hub assertion
// for each upgrade, as the Talaria hub does. It runs only with HERMES_MCP_E2E=1
// and hermes on PATH.
package mcpe2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/internal/mcp"
	"github.com/USA-RedDragon/hermes-device/internal/mcp/mcptest"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

func TestMain(m *testing.M) {
	mcptest.Main()
	os.Exit(m.Run())
}

const (
	backendID = "alice"
	deviceID  = "d_e2e"
)

// hub dials the plugin's exec route with a fresh hub assertion.
type hub struct {
	url string
	key ed25519.PrivateKey
}

func (h hub) DialLeg(ctx context.Context, _ string) (*websocket.Conn, error) {
	now := time.Now()
	tok, err := assertion.Sign(h.key, assertion.Claims{
		Aud: backendID, Leg: assertion.LegExec, Method: http.MethodGet, Path: protocol.PluginExecPath,
		Sub: "alice", DeviceID: deviceID, DeviceName: "desktop", IssuedAt: now.Unix(), Expires: now.Add(30 * time.Second).Unix(),
	})
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	hdr.Set(assertion.Header, tok)
	c, _, err := websocket.Dial(ctx, h.url+protocol.PluginExecPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		c.SetReadLimit(64 << 20)
	}
	return c, err
}

func TestMCPEndToEnd(t *testing.T) { //nolint:paralleltest // one plugin server at a time
	if os.Getenv("HERMES_MCP_E2E") != "1" {
		t.Skip("set HERMES_MCP_E2E=1 to run the device plugin in the Hermes runtime")
	}
	if _, err := exec.LookPath("hermes"); err != nil {
		t.Skip("hermes is not on PATH; the plugin needs the Hermes runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	portFile := filepath.Join(dir, "port")
	pidFile := filepath.Join(dir, "pids")

	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tests := filepath.Join(repo, "plugin", "device", "tests")
	py := exec.CommandContext(ctx, "sh", filepath.Join(repo, "scripts", "test-plugin.sh"), //nolint:gosec // repo script
		"-s", tests, "-t", tests, "-p", "test_mcp_e2e.py", "-v")
	py.Env = append(os.Environ(), "HERMES_MCP_E2E_DIR="+dir, "HERMES_MCP_E2E_ROOT="+root,
		"HERMES_MCP_E2E_KEY="+base64.StdEncoding.EncodeToString(pub))
	var out strings.Builder
	py.Stdout, py.Stderr = &out, &out
	if err := py.Start(); err != nil {
		t.Fatal(err)
	}
	pyDone := make(chan error, 1)
	go func() { pyDone <- py.Wait() }()

	var port int
	for port == 0 {
		select {
		case err := <-pyDone:
			t.Fatalf("plugin test ended before it listened: %v\n%s", err, out.String())
		case <-time.After(50 * time.Millisecond):
		}
		if b, err := os.ReadFile(portFile); err == nil { //nolint:gosec // test file
			port, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.New(mcp.Options{Servers: map[string]mcp.ServerConfig{
		"fake": {Command: exe, Env: map[string]string{mcptest.Env: "1", mcptest.PIDFileEnv: pidFile, "FAKE_SECRET": "from-config"}},
	}, Dir: root, ClientVersion: "e2e"})
	allow, err := allowlist.New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := device.New(device.Options{
		Hub: hub{url: "ws://127.0.0.1:" + strconv.Itoa(port), key: key}, Allow: allow, MCP: mgr, ClientVersion: "e2e",
		TempDir: root, MinBackoff: 100 * time.Millisecond, MaxBackoff: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	devCtx, devCancel := context.WithCancel(ctx)
	devDone := make(chan error, 1)
	go func() { devDone <- dev.Run(devCtx) }()

	pyErr := <-pyDone
	t.Logf("plugin side:\n%s", out.String())
	devCancel()
	if err := <-devDone; err != nil {
		t.Errorf("device Run: %v", err)
	}
	mgr.Close()
	if pyErr != nil {
		t.Fatalf("plugin side failed: %v", pyErr)
	}

	pids, err := os.ReadFile(pidFile) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(pids))
	if len(lines) < 2 {
		t.Fatalf("want a restart after the crash, pids %q", lines)
	}
	for _, l := range lines {
		pid, _ := strconv.Atoi(l)
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			t.Errorf("MCP server %d still runs after the daemon stopped", pid)
		}
	}
}
