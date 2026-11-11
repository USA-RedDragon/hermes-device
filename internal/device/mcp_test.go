package device_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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

const serverFake = "fake"

func fakeMCP(t *testing.T) *mcp.Manager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := mcp.New(mcp.Options{Servers: map[string]mcp.ServerConfig{
		serverFake: {Command: exe, Env: map[string]string{mcptest.Env: "1"}},
	}, Dir: t.TempDir()})
	t.Cleanup(m.Close)
	return m
}

func TestMCPHelloOffersServers(t *testing.T) {
	t.Parallel()
	hellos := make(chan protocol.Hello, 2)
	f := &fakeHub{handler: func(ctx context.Context, _ *websocket.Conn, h protocol.Hello) {
		hellos <- h
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.MCP = mcp.New(mcp.Options{})
	start(t, opts)
	if h := recv(t, hellos); !slices.Equal(h.Capabilities, []string{protocol.CapabilityDetach}) || len(h.MCPServers) != 0 {
		t.Errorf("hello without servers = %+v", h)
	}
	opts = options(t, serve(t, f).URL, t.TempDir())
	opts.MCP = fakeMCP(t)
	opts.Browser = newFakeBrowser(false)
	start(t, opts)
	h := recv(t, hellos)
	if strings.Join(h.Capabilities, ",") != "detach,browser,mcp" || strings.Join(h.MCPServers, ",") != serverFake {
		t.Errorf("hello = %+v", h)
	}
}

func TestMCPRequests(t *testing.T) {
	t.Parallel()
	results := make(chan map[string]protocol.MCPResult, 1)
	reqs := []protocol.MCP{
		{ID: "list", Op: protocol.MCPOpList},
		{ID: "tools", Op: protocol.MCPOpTools, Server: serverFake},
		{ID: "echo", Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolEcho, Arguments: json.RawMessage(`{"text":"<a href=x>&</a>"}`)},
		{ID: "other", Op: protocol.MCPOpCall, Server: "rea", Tool: mcptest.ToolEcho},
		{ID: "op", Op: "prompts", Server: serverFake},
		{ID: "args", Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolEcho, Arguments: json.RawMessage(`[1]`)},
		{ID: "late", Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolSlow, Timeout: 0.2},
		{ID: idKill, Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolSlow, Timeout: 30},
		{ID: "huge", Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolEcho,
			Arguments: json.RawMessage(`{"text":"` + strings.Repeat("a", protocol.MaxMCPRequest) + `"}`)},
	}
	f := &fakeHub{capabilities: []string{protocol.CapabilityMCP}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		writeJSON(ctx, conn, map[string]any{"type": protocol.TypeMCP, "op": protocol.MCPOpList})
		for _, r := range reqs {
			r.Type = protocol.TypeMCP
			writeJSON(ctx, conn, r)
		}
		got := map[string]protocol.MCPResult{}
		for len(got) < len(reqs) {
			var r protocol.MCPResult
			if !readType(ctx, t, conn, protocol.TypeMCPResult, &r) {
				return
			}
			got[r.ID] = r
			if r.ID == "echo" {
				writeJSON(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: idKill})
			}
		}
		results <- got
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	opts.MCP = fakeMCP(t)
	start(t, opts)
	got := recv(t, results)
	want := map[string]string{
		"other": protocol.MCPUnknownServer, "op": protocol.MCPRefused, "args": protocol.MCPRefused,
		"late": protocol.MCPTimeout, idKill: protocol.MCPCancelled, "huge": protocol.MCPRefused,
	}
	for id, code := range want {
		if got[id].OK || got[id].Code != code {
			t.Errorf("%s: %+v, want code %s", id, got[id], code)
		}
	}
	if r := got["echo"]; !r.OK || !strings.Contains(string(r.Result), `"text":"<a href=x>&</a>"`) {
		t.Errorf("echo: %+v (%s)", r, r.Result)
	}
	if r := got["tools"]; !r.OK || !strings.Contains(string(r.Result), mcptest.ToolInspect) {
		t.Errorf("tools: %+v (%s)", r, r.Result)
	}
	if r := got["list"]; !r.OK || !strings.Contains(string(r.Result), `"name":"fake"`) {
		t.Errorf("list: %+v (%s)", r, r.Result)
	}
}

// A device whose welcome did not accept the capability refuses MCP
// requests, and never starts a server for them.
func TestMCPWithoutCapability(t *testing.T) {
	t.Parallel()
	got := make(chan protocol.MCPResult, 1)
	f := &fakeHub{handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		writeJSON(ctx, conn, protocol.MCP{Type: protocol.TypeMCP, ID: "x", Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolEcho})
		var r protocol.MCPResult
		if readType(ctx, t, conn, protocol.TypeMCPResult, &r) {
			got <- r
		}
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, t.TempDir())
	m := fakeMCP(t)
	opts.MCP = m
	start(t, opts)
	if r := recv(t, got); r.OK || r.Code != protocol.MCPRefused {
		t.Errorf("result = %+v", r)
	}
	if m.List()[0].Running {
		t.Error("server started on a leg without the capability")
	}
}

// Busy covers commands until the plugin confirms their exit, and MCP calls
// in flight. Drain refuses only while the device is idle, and then refuses
// new work.
func TestBusyAndDrain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	step := make(chan string)
	next := make(chan struct{})
	f := &fakeHub{capabilities: []string{protocol.CapabilityMCP}, handler: func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		msgs := make(chan []byte, 64)
		go func() {
			defer close(msgs)
			for {
				typ, data, err := readMsg(ctx, conn)
				if err != nil {
					return
				}
				if typ == protocol.TypePing {
					var p protocol.Ping
					_ = json.Unmarshal(data, &p)
					writeJSON(ctx, conn, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
					continue
				}
				msgs <- data
			}
		}()
		await := func(typ, id string) map[string]any {
			for data := range msgs {
				var m map[string]any
				_ = json.Unmarshal(data, &m)
				if m["type"] == typ && m["id"] == id {
					return m
				}
			}
			return nil
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "sleep 0.5", Cwd: root, Timeout: 10})
		step <- "exec"
		<-next
		await(protocol.TypeExit, "1")
		step <- "exited"
		<-next
		writeJSON(ctx, conn, protocol.MCP{Type: protocol.TypeMCP, ID: "in-flight", Op: protocol.MCPOpCall, Server: serverFake, Tool: mcptest.ToolSlow, Timeout: 0.5})
		step <- "mcp"
		<-next
		await(protocol.TypeMCPResult, "in-flight")
		step <- "mcp done"
		<-next
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "2", Cmd: "true", Cwd: root, Timeout: 10})
		writeJSON(ctx, conn, protocol.MCP{Type: protocol.TypeMCP, ID: "after-drain", Op: protocol.MCPOpList})
		if e := await(protocol.TypeExit, "2"); e["code"] != float64(protocol.ExitRefused) || !strings.Contains(fmt.Sprint(e["error"]), "restarting for an update") {
			t.Errorf("exec after drain = %v", e)
		}
		if r := await(protocol.TypeMCPResult, "after-drain"); r["ok"] == true || !strings.Contains(fmt.Sprint(r["error"]), "restarting for an update") {
			t.Errorf("MCP after drain = %v", r)
		}
		step <- "refused"
		<-ctx.Done()
	}}
	opts := options(t, serve(t, f).URL, root)
	opts.MCP = fakeMCP(t)
	opts.PingInterval = 50 * time.Millisecond
	c, err := device.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Logf("Run returned after cancel: %v", err)
		}
	})
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-step:
			if got != want {
				t.Fatalf("step %q, want %q", got, want)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	expect("exec")
	waitFor(t, "the command to register", c.Busy)
	if c.Drain() {
		t.Fatal("Drain with a running command")
	}
	next <- struct{}{}
	expect("exited")
	waitFor(t, "the plugin to confirm the exit", func() bool { return !c.Busy() })
	next <- struct{}{}
	expect("mcp")
	waitFor(t, "the MCP call to start", c.Busy)
	if c.Drain() {
		t.Fatal("Drain with an MCP call in flight")
	}
	next <- struct{}{}
	expect("mcp done")
	waitFor(t, "the MCP call to end", func() bool { return !c.Busy() })
	if !c.Drain() {
		t.Fatal("Drain refused an idle device")
	}
	next <- struct{}{}
	expect("refused")
}
