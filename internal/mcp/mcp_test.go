package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/mcp"
	"github.com/USA-RedDragon/hermes-device/internal/mcp/mcptest"
)

const fake = "fake"

func TestMain(m *testing.M) {
	mcptest.Main()
	os.Exit(m.Run())
}

func newManager(t *testing.T, opts mcp.Options) *mcp.Manager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Servers == nil {
		opts.Servers = map[string]mcp.ServerConfig{fake: {Command: exe, Env: map[string]string{mcptest.Env: "1", "FAKE_SECRET": "s3"}}}
	}
	opts.Env = append(os.Environ(), "FAKE_SECRET=overridden")
	opts.Dir = t.TempDir()
	m := mcp.New(opts)
	t.Cleanup(m.Close)
	return m
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type toolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func callText(ctx context.Context, t *testing.T, m *mcp.Manager, tool, args string) string {
	t.Helper()
	raw, err := m.Call(ctx, fake, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	var r toolResult
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Content) == 0 {
		t.Fatalf("call %s: result %s", tool, raw)
	}
	return r.Content[0].Text
}

func code(err error) string {
	var me *mcp.Error
	if errors.As(err, &me) {
		return me.Code
	}
	return ""
}

func toolNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Tools))
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func TestLazyStartToolsAndCalls(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{})
	if st := m.List(); len(st) != 1 || st[0].Running {
		t.Fatalf("started before use: %+v", st)
	}
	raw, err := m.Tools(ctx, fake)
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, raw)
	if len(names) != 10 || names[0] != mcptest.ToolEcho || names[9] != mcptest.ToolInspect {
		t.Fatalf("tools from both pages: %v", names)
	}
	if st := m.List(); !st[0].Running || st[0].Server != "fake 1.0.0" {
		t.Fatalf("status %+v", st)
	}
	if got := callText(ctx, t, m, mcptest.ToolEcho, `{"text":"hi"}`); got != "hi" {
		t.Fatalf("echo %q", got)
	}
	if got := callText(ctx, t, m, mcptest.ToolEnv, `{"name":"FAKE_SECRET"}`); got != "s3" {
		t.Fatalf("configured env must win over the daemon's: %q", got)
	}
	if got := callText(ctx, t, m, mcptest.ToolRoots, `{}`); !strings.Contains(got, "-32601") {
		t.Fatalf("server requests other than ping must be refused: %q", got)
	}
	raw, err = m.Call(ctx, fake, mcptest.ToolFail, nil)
	if err != nil || !strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("a tool error is a result: %s %v", raw, err)
	}
	if _, err := m.Call(ctx, fake, "nope", nil); code(err) != mcp.CodeFailed || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("JSON-RPC error: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{})
	if _, err := m.Tools(ctx, "other"); code(err) != mcp.CodeUnknownServer {
		t.Fatalf("unlisted server: %v", err)
	}
	if _, err := m.Call(ctx, "../fake", mcptest.ToolEcho, nil); code(err) != mcp.CodeUnknownServer {
		t.Fatalf("unlisted server: %v", err)
	}
	for _, args := range []string{`[1]`, `"x"`, `{"a":`} {
		if _, err := m.Call(ctx, fake, mcptest.ToolEcho, json.RawMessage(args)); code(err) != mcp.CodeRefused {
			t.Fatalf("arguments %s: %v", args, err)
		}
	}
	if _, err := m.Call(ctx, fake, strings.Repeat("a", 129), nil); code(err) != mcp.CodeRefused {
		t.Fatalf("long tool name: %v", err)
	}
	if st := m.List(); st[0].Running {
		t.Fatal("a refused request must not start the server")
	}
}

func TestListChanged(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{})
	raw, err := m.Tools(ctx, fake)
	if err != nil || len(toolNames(t, raw)) != 10 {
		t.Fatal(raw, err)
	}
	callText(ctx, t, m, mcptest.ToolChange, `{}`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err = m.Tools(ctx, fake)
		if err != nil {
			t.Fatal(err)
		}
		if len(toolNames(t, raw)) == 11 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool list not refreshed: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCancelAndTimeout(t *testing.T) {
	t.Parallel()
	m := newManager(t, mcp.Options{})
	ctx := ctxT(t)
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := m.Call(short, fake, mcptest.ToolSlow, nil); code(err) != mcp.CodeTimeout {
		t.Fatalf("timeout: %v", err)
	}
	c2, cancel2 := context.WithCancel(ctx)
	go func() { time.Sleep(100 * time.Millisecond); cancel2() }()
	if _, err := m.Call(c2, fake, mcptest.ToolSlow, nil); code(err) != mcp.CodeCancelled {
		t.Fatalf("cancel: %v", err)
	}
	if got := callText(ctx, t, m, mcptest.ToolCount, `{}`); got != "2" {
		t.Fatalf("the server must keep running after a cancel, count %s", got)
	}
}

func TestCrashRestartsWithBackoff(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{})
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
	_, err := m.Call(ctx, fake, mcptest.ToolCrash, nil)
	if code(err) != mcp.CodeFailed || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("crash: %v", err)
	}
	_, err = m.Call(ctx, fake, mcptest.ToolCount, nil)
	if code(err) != mcp.CodeUnavailable || !strings.Contains(err.Error(), "crashing on purpose") {
		t.Fatalf("a quick crash must back off and name the last stderr line: %v", err)
	}
	st := m.List()[0]
	if st.Running || st.RetryIn < 1 || !strings.Contains(st.LastError, "exit status 3") {
		t.Fatalf("status after crash: %+v", st)
	}
	time.Sleep(1100 * time.Millisecond)
	if got := callText(ctx, t, m, mcptest.ToolCount, `{}`); got != "1" {
		t.Fatalf("restarted server count %s", got)
	}
}

func TestBadCommand(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{Servers: map[string]mcp.ServerConfig{fake: {Command: "/nonexistent/mcp-server"}}})
	if _, err := m.Tools(ctx, fake); code(err) != mcp.CodeUnavailable {
		t.Fatalf("missing program: %v", err)
	}
}

func TestOversizedAnswer(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{MaxMessage: 64 << 10})
	if _, err := m.Call(ctx, fake, mcptest.ToolBig, json.RawMessage(`{"bytes":200000}`)); code(err) != mcp.CodeTooLarge {
		t.Fatalf("oversized answer: %v", err)
	}
	if got := callText(ctx, t, m, mcptest.ToolCount, `{}`); got != "1" {
		t.Fatalf("an oversized answer with its id first must not kill the server: count %s", got)
	}
	if _, err := m.Call(ctx, fake, mcptest.ToolBig, json.RawMessage(`{"bytes":200000,"id_last":true}`)); code(err) != mcp.CodeTooLarge {
		t.Fatalf("oversized answer with its id last: %v", err)
	}
	if got := callText(ctx, t, m, mcptest.ToolCount, `{}`); got != "2" {
		t.Fatalf("an oversized answer with its id last must not kill the server: count %s", got)
	}
	if got := callText(ctx, t, m, mcptest.ToolBig, `{"bytes":1000,"id_last":true}`); len(got) != 1000 {
		t.Fatalf("small answer with id last %d", len(got))
	}
	_, err := m.Call(ctx, fake, mcptest.ToolBig, json.RawMessage(`{"bytes":200000,"garbage":true}`))
	if code(err) != mcp.CodeFailed || !strings.Contains(err.Error(), "over 65536 bytes") {
		t.Fatalf("an oversized line without an id must end the server: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if got := callText(ctx, t, m, mcptest.ToolBig, `{"bytes":1000}`); len(got) != 1000 {
		t.Fatalf("small answer %d", len(got))
	}
}

func TestBusy(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{MaxCalls: 1})
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
	slow, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	start := func() { go func() { _, err := m.Call(slow, fake, mcptest.ToolSlow, nil); done <- err }() }
	start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		// A probe sent at once can take the slot before the slow call does, every time.
		time.Sleep(10 * time.Millisecond)
		select {
		case err := <-done:
			if code(err) != mcp.CodeBusy {
				t.Fatalf("slow call: %v", err)
			}
			start()
			continue
		default:
		}
		_, err := m.Call(ctx, fake, mcptest.ToolCount, nil)
		if code(err) == mcp.CodeBusy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no busy: %v", err)
		}
	}
	cancel()
	if err := <-done; code(err) != mcp.CodeCancelled {
		t.Fatalf("slow call: %v", err)
	}
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
}

func TestIdleStopAndClose(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{IdleTimeout: 200 * time.Millisecond})
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
	deadline := time.Now().Add(10 * time.Second)
	for m.List()[0].Running {
		if time.Now().After(deadline) {
			t.Fatal("idle server not stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := m.List()[0]; st.LastError != "" || st.RetryIn != 0 {
		t.Fatalf("an idle stop is not a failure: %+v", st)
	}
	if got := callText(ctx, t, m, mcptest.ToolCount, `{}`); got != "1" {
		t.Fatalf("started again after idle: count %s", got)
	}
	m.Close()
	if st := m.List()[0]; st.Running {
		t.Fatal("running after Close")
	}
	if _, err := m.Call(ctx, fake, mcptest.ToolCount, nil); code(err) != mcp.CodeUnavailable {
		t.Fatalf("after Close: %v", err)
	}
}

func TestServerRequestFloodEndsServer(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	m := newManager(t, mcp.Options{})
	callText(ctx, t, m, mcptest.ToolCount, `{}`)
	began := time.Now()
	_, err := m.Call(ctx, fake, mcptest.ToolFlood, nil)
	if code(err) != mcp.CodeFailed || !strings.Contains(err.Error(), "messages waited for its stdin") {
		t.Fatalf("flood: %v", err)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Fatalf("flood took %s to end", d)
	}
}

func TestHungInitializeDoesNotHoldCallers(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, mcp.Options{InitTimeout: 2 * time.Second, Servers: map[string]mcp.ServerConfig{
		fake: {Command: exe, Env: map[string]string{mcptest.Env: "1", mcptest.HangInitEnv: "1"}},
	}})
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := m.Tools(short, fake); code(err) != mcp.CodeTimeout {
		t.Fatalf("hung initialize: %v", err)
	}
	if d := time.Since(began); d > time.Second {
		t.Fatalf("caller waited %s for a start it gave up on", d)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := m.List()[0]
		if strings.Contains(st.LastError, "initialize") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no initialize failure: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := m.Tools(ctxT(t), fake); code(err) != mcp.CodeUnavailable {
		t.Fatalf("after the failed start: %v", err)
	}
}

func TestServerIdleTimeoutOverrides(t *testing.T) {
	t.Parallel()
	ctx := ctxT(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	never, short := time.Duration(0), 200*time.Millisecond
	env := map[string]string{mcptest.Env: "1"}
	m := newManager(t, mcp.Options{IdleTimeout: 200 * time.Millisecond, Servers: map[string]mcp.ServerConfig{
		"keep": {Command: exe, Env: env, IdleTimeout: &never},
		"stop": {Command: exe, Env: env, IdleTimeout: &short},
	}})
	for _, name := range []string{"keep", "stop"} {
		if _, err := m.Call(ctx, name, mcptest.ToolCount, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	running := func(name string) bool {
		for _, st := range m.List() {
			if st.Name == name {
				return st.Running
			}
		}
		t.Fatalf("no server %s", name)
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for running("stop") {
		if time.Now().After(deadline) {
			t.Fatal("server with a short idle-timeout not stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	if !running("keep") {
		t.Fatal("server with idle-timeout 0 stopped although the global timeout passed")
	}
}
