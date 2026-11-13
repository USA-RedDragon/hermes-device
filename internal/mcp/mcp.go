// Package mcp runs the MCP servers listed in the device config and acts as
// their MCP client over stdio: newline-delimited JSON-RPC 2.0, as the MCP
// stdio transport defines it. Only the tools part of MCP is used. A server
// starts on its first use, stops after it has been idle for a while, starts
// again on the next use after it exits, and stops with the Manager
// (docs/protocol.md section 4.9).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/USA-RedDragon/talaria/internal/proctree"
	"github.com/USA-RedDragon/talaria/protocol"
)

// ProtocolVersion is the MCP revision the client asks for in initialize.
const ProtocolVersion = "2025-06-18"

const jsonrpcVersion = "2.0"

// Defaults of Options.
const (
	DefaultInitTimeout = 120 * time.Second
	DefaultMaxCalls    = 8
	// DefaultMaxTotalCalls is the most requests in flight on all servers
	// together, which bounds the memory that large answers take.
	DefaultMaxTotalCalls = 16
	// writeQueue is the most messages waiting for a server's stdin.
	writeQueue = 64
	// stderrBurst is the most stderr lines a server may log per second;
	// the rest are counted and reported.
	stderrBurst = 50
	// StopGrace is how long a server gets after its stdin closes, and
	// again after SIGTERM, before the next step.
	StopGrace = 3 * time.Second
	// StableAfter is how long a server must run for an exit to count as
	// a crash after a good run instead of a failed start.
	StableAfter = 30 * time.Second
	// MaxBackoff caps the wait before a server that keeps failing may
	// start again.
	MaxBackoff = 60 * time.Second
	maxPages   = 50
	maxStderr  = 2000
	writeLimit = 30 * time.Second
)

// Error codes of an Error. They are the codes of an mcp_result.
const (
	CodeUnknownServer = protocol.MCPUnknownServer
	CodeUnavailable   = protocol.MCPUnavailable
	CodeBusy          = protocol.MCPBusy
	CodeTooLarge      = protocol.MCPTooLarge
	CodeTimeout       = protocol.MCPTimeout
	CodeCancelled     = protocol.MCPCancelled
	CodeFailed        = protocol.MCPFailed
	CodeRefused       = protocol.MCPRefused
)

// Error is a failed MCP request.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ServerConfig is one server of the device config.
type ServerConfig struct {
	Command string
	Args    []string
	Env     map[string]string
	Dir     string
	// IdleTimeout overrides Options.IdleTimeout for this server; nil uses
	// it.
	IdleTimeout *time.Duration
}

// NameRE is what a server name must match.
var NameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Options configure a Manager.
type Options struct {
	Servers map[string]ServerConfig
	// Env is the environment every server starts with, before its own
	// Env. Defaults to os.Environ.
	Env []string
	// Dir is the working directory of a server without its own. Defaults
	// to $HOME.
	Dir string
	// IdleTimeout stops a server with no request in flight after this
	// long. 0 keeps it running until the Manager closes.
	IdleTimeout time.Duration
	InitTimeout time.Duration
	// MaxCalls is the most requests in flight per server, and
	// MaxTotalCalls on all servers together.
	MaxCalls      int
	MaxTotalCalls int
	// MaxMessage is the largest message a server may send, in bytes.
	MaxMessage    int
	ClientVersion string
}

// Manager owns the configured servers.
type Manager struct {
	opts    Options
	servers map[string]*server
	closed  atomic.Bool
	// total holds one token per request in flight on any server.
	total chan struct{}
}

// New returns a Manager. It starts nothing.
func New(opts Options) *Manager {
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	if opts.Dir == "" {
		opts.Dir = os.Getenv("HOME")
	}
	if opts.InitTimeout <= 0 {
		opts.InitTimeout = DefaultInitTimeout
	}
	if opts.MaxCalls <= 0 {
		opts.MaxCalls = DefaultMaxCalls
	}
	if opts.MaxTotalCalls <= 0 {
		opts.MaxTotalCalls = DefaultMaxTotalCalls
	}
	if opts.MaxMessage <= 0 {
		opts.MaxMessage = protocol.MaxMCPResult
	}
	m := &Manager{opts: opts, servers: map[string]*server{}, total: make(chan struct{}, opts.MaxTotalCalls)}
	for name, cfg := range opts.Servers {
		m.servers[name] = &server{m: m, name: name, cfg: cfg}
	}
	return m
}

// Names returns the configured server names, sorted.
func (m *Manager) Names() []string { return slices.Sorted(maps.Keys(m.servers)) }

// Status is the state of one server, for the list operation.
type Status struct {
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	Server    string `json:"server,omitempty"`
	LastError string `json:"last_error,omitempty"`
	// RetryIn is the number of seconds before a failed server may start
	// again.
	RetryIn int `json:"retry_in,omitempty"`
}

// List returns every server's state, sorted by name.
func (m *Manager) List() []Status {
	out := make([]Status, 0, len(m.servers))
	for _, name := range m.Names() {
		out = append(out, m.servers[name].status())
	}
	return out
}

func (m *Manager) server(name string) (*server, error) {
	s, ok := m.servers[name]
	if !ok {
		return nil, fail(CodeUnknownServer, "no MCP server named %q in this device's config", cut(name, 64))
	}
	return s, nil
}

// Tools returns the server's tools as {"tools": [...]}, every page of
// tools/list joined. The list is kept until the server says it changed or
// exits.
func (m *Manager) Tools(ctx context.Context, name string) (json.RawMessage, error) {
	s, err := m.server(name)
	if err != nil {
		return nil, err
	}
	return s.tools(ctx)
}

// Call runs tools/call and returns the server's CallToolResult unchanged.
// args must be a JSON object or empty.
func (m *Manager) Call(ctx context.Context, name, tool string, args json.RawMessage) (json.RawMessage, error) {
	s, err := m.server(name)
	if err != nil {
		return nil, err
	}
	if tool == "" || len(tool) > 128 {
		return nil, fail(CodeRefused, "the tool name must be 1 to 128 bytes")
	}
	args = bytes.TrimSpace(args)
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if args[0] != '{' || !json.Valid(args) {
		return nil, fail(CodeRefused, "the arguments must be a JSON object")
	}
	params, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{tool, args})
	if err != nil {
		return nil, fail(CodeRefused, "arguments: %v", err)
	}
	return s.request(ctx, "tools/call", params)
}

// Close stops every server and waits for them to exit. Later requests
// fail.
func (m *Manager) Close() {
	m.closed.Store(true)
	var wg sync.WaitGroup
	for _, s := range m.servers {
		wg.Go(func() { s.stop("the daemon stopped") })
	}
	wg.Wait()
}

type server struct {
	m    *Manager
	name string
	cfg  ServerConfig

	mu       sync.Mutex
	proc     *proc
	starting chan struct{}
	active   int
	idle     *time.Timer
	failures int
	retryAt  time.Time
	lastErr  string
	toolList json.RawMessage
}

func (s *server) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Name: s.name, Running: s.proc != nil, LastError: s.lastErr}
	if s.proc != nil {
		st.Server = s.proc.serverInfo
	}
	if d := time.Until(s.retryAt); d > 0 {
		st.RetryIn = int(d.Round(time.Second) / time.Second)
	}
	return st
}

// acquire counts one request in flight, or fails when MaxCalls are.
func (s *server) acquire() error {
	select {
	case s.m.total <- struct{}{}:
	default:
		return fail(CodeBusy, "this device already runs %d MCP requests", s.m.opts.MaxTotalCalls)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= s.m.opts.MaxCalls {
		<-s.m.total
		return fail(CodeBusy, "MCP server %s already runs %d requests", s.name, s.m.opts.MaxCalls)
	}
	s.active++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	return nil
}

// idleTimeout is how long the server may sit without a request.
func (s *server) idleTimeout() time.Duration {
	if s.cfg.IdleTimeout != nil {
		return *s.cfg.IdleTimeout
	}
	return s.m.opts.IdleTimeout
}

func (s *server) release() {
	<-s.m.total
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	idle := s.idleTimeout()
	if s.active > 0 || s.proc == nil || idle <= 0 {
		return
	}
	p := s.proc
	s.idle = time.AfterFunc(idle, func() {
		s.mu.Lock()
		current := s.proc == p && s.active == 0
		if current {
			s.detachLocked()
		}
		s.mu.Unlock()
		if current {
			slog.Info("Stopping idle MCP server", "server", s.name, "idle", idle)
			p.stop()
		}
	})
}

// ensure returns the running server, starting it when it is not running.
func (s *server) ensure(ctx context.Context) (*proc, error) {
	for {
		if s.m.closed.Load() {
			return nil, fail(CodeUnavailable, "the daemon is stopping")
		}
		s.mu.Lock()
		if p := s.proc; p != nil {
			s.mu.Unlock()
			if !p.hasExited() {
				return p, nil
			}
			<-p.done
			s.reap(p)
			continue
		}
		if wait := s.starting; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctxError(ctx, s.name)
			}
		}
		if d := time.Until(s.retryAt); d > 0 {
			msg := fmt.Sprintf("MCP server %s failed (%s); it may start again in %ds", s.name, s.lastErr,
				int(d.Round(time.Second)/time.Second))
			s.mu.Unlock()
			return nil, &Error{Code: CodeUnavailable, Message: msg}
		}
		done := make(chan struct{})
		s.starting = done
		s.mu.Unlock()
		// The start runs on its own, so a caller whose context ends stops
		// waiting for it while the server keeps starting for the next one.
		go s.launch(done) //nolint:contextcheck // the server outlives the request that starts it
	}
}

func (s *server) launch(done chan struct{}) {
	p, err := s.start()
	s.mu.Lock()
	s.starting = nil
	closing := s.m.closed.Load()
	switch {
	case err != nil:
		s.failedLocked("did not start: " + err.Error())
		slog.Warn("MCP server did not start", "server", s.name, "error", err)
	case !closing:
		s.proc = p
		s.lastErr = ""
	}
	s.mu.Unlock()
	close(done)
	switch {
	case err != nil:
	case closing:
		p.stop()
	default:
		go s.watch(p)
	}
}

func (s *server) failedLocked(why string) {
	s.failures++
	s.lastErr = cut(why, 300)
	s.retryAt = time.Now().Add(min(time.Second<<min(s.failures-1, 6), MaxBackoff))
}

// watch forgets p once it exits. An exit that the daemon did not ask for
// is a crash: the next request starts the server again, after a backoff
// when it crashed soon after it started.
func (s *server) watch(p *proc) {
	<-p.done
	s.reap(p)
}

func (s *server) reap(p *proc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != p {
		return
	}
	s.detachLocked()
	if p.stopping.Load() {
		return
	}
	why := p.exitReason()
	if time.Since(p.started) >= StableAfter {
		s.failures = 0
	}
	s.failedLocked(why)
	slog.Warn("MCP server exited", "server", s.name, "reason", why, "retry_in", time.Until(s.retryAt).Round(time.Second))
}

// detachLocked forgets the running server so that the next request starts
// a new one while the old one stops.
func (s *server) detachLocked() {
	s.proc = nil
	s.toolList = nil
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
}

func (s *server) stop(why string) {
	s.mu.Lock()
	starting := s.starting
	s.mu.Unlock()
	if starting != nil {
		<-starting
	}
	s.mu.Lock()
	p := s.proc
	s.detachLocked()
	s.mu.Unlock()
	if p != nil {
		slog.Info("Stopping MCP server", "server", s.name, "why", why)
		p.stop()
	}
}

func (s *server) tools(ctx context.Context) (json.RawMessage, error) {
	if err := s.acquire(); err != nil {
		return nil, err
	}
	defer s.release()
	p, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cached := s.toolList
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	gen := p.toolsGen.Load()
	var all []json.RawMessage
	size := 0
	cursor := ""
	for range maxPages {
		params := json.RawMessage(`{}`)
		if cursor != "" {
			var err error
			if params, err = json.Marshal(map[string]string{"cursor": cursor}); err != nil {
				return nil, fail(CodeFailed, "tools/list: %v", err)
			}
		}
		raw, err := p.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fail(CodeFailed, "MCP server %s sent a tools/list result that is not valid: %v", s.name, err)
		}
		for _, t := range page.Tools {
			size += len(t) + 1
		}
		if size > s.m.opts.MaxMessage {
			return nil, fail(CodeTooLarge, "MCP server %s lists tools over %d bytes", s.name, s.m.opts.MaxMessage)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			cursor = ""
			break
		}
		cursor = page.NextCursor
	}
	if cursor != "" {
		return nil, fail(CodeTooLarge, "MCP server %s lists more than %d pages of tools", s.name, maxPages)
	}
	if all == nil {
		all = []json.RawMessage{}
	}
	out, err := json.Marshal(map[string][]json.RawMessage{"tools": all})
	if err != nil {
		return nil, fail(CodeFailed, "tools: %v", err)
	}
	s.mu.Lock()
	if s.proc == p && p.toolsGen.Load() == gen {
		s.toolList = out
	}
	s.mu.Unlock()
	return out, nil
}

func (s *server) request(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if err := s.acquire(); err != nil {
		return nil, err
	}
	defer s.release()
	p, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return p.call(ctx, method, params)
}

func (s *server) toolsChanged(p *proc) {
	p.toolsGen.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == p {
		s.toolList = nil
	}
}

func ctxError(ctx context.Context, name string) *Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fail(CodeTimeout, "MCP server %s did not answer in time", name)
	}
	return fail(CodeCancelled, "the request to MCP server %s was cancelled", name)
}

// proc is one running server process.
type proc struct {
	s          *server
	cmd        *exec.Cmd
	tree       *proctree.Tree
	stdin      *os.File
	out        chan outMsg
	gmu        sync.Mutex
	reaped     bool
	nextID     atomic.Int64
	pmu        sync.Mutex
	pending    map[int64]chan rpcMessage
	exited     bool
	done       chan struct{}
	waitErr    error
	stopping   atomic.Bool
	stopOnce   sync.Once
	toolsGen   atomic.Int64
	started    time.Time
	serverInfo string
	lastStderr atomic.Value
	fatal      atomic.Value
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
	// local is a failure the client itself reports for the request.
	local *Error
}

func (s *server) env() []string {
	env := slices.Clone(s.m.opts.Env)
	for _, k := range slices.Sorted(maps.Keys(s.cfg.Env)) {
		env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
		env = append(env, k+"="+s.cfg.Env[k])
	}
	return env
}

// start runs the server and does the initialize handshake.
func (s *server) start() (*proc, error) {
	// No context kills the server: stop ends its process group.
	path, err := exec.LookPath(s.cfg.Command)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: append([]string{s.cfg.Command}, s.cfg.Args...)}
	cmd.Env = s.env()
	cmd.Dir = s.cfg.Dir
	if cmd.Dir == "" {
		cmd.Dir = s.m.opts.Dir
	}
	proctree.Prepare(cmd)
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = cmd.Start()
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()
	var tree *proctree.Tree
	if err == nil {
		if tree, err = proctree.Attach(cmd); err != nil {
			_ = cmd.Wait()
		}
	}
	if err != nil {
		_ = inW.Close()
		_ = outR.Close()
		_ = errR.Close()
		return nil, err
	}
	p := &proc{s: s, cmd: cmd, tree: tree, stdin: inW, out: make(chan outMsg, writeQueue), pending: map[int64]chan rpcMessage{},
		done: make(chan struct{}), started: time.Now()}
	slog.Info("Started MCP server", "server", s.name, "pid", cmd.Process.Pid)
	go p.writer()
	readers := make(chan struct{}, 2)
	go func() { p.readStdout(outR); readers <- struct{}{} }()
	go func() { p.readStderr(errR); readers <- struct{}{} }()
	go func() {
		p.waitErr = cmd.Wait()
		// The group may hold children that keep the pipes open. After this
		// the group id may be reused, so nothing signals it again.
		p.gmu.Lock()
		_ = tree.Kill()
		tree.Release()
		p.reaped = true
		p.gmu.Unlock()
		for range 2 {
			select {
			case <-readers:
			case <-time.After(time.Second):
				_ = outR.Close()
				_ = errR.Close()
			}
		}
		_ = inW.Close()
		p.failAll(fail(CodeFailed, "MCP server %s exited (%s); the next request starts it again, without the state it had",
			s.name, p.exitReason()))
		close(p.done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), s.m.opts.InitTimeout)
	defer cancel()
	params, err := json.Marshal(map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "talaria", "version": s.m.opts.ClientVersion},
	})
	var raw json.RawMessage
	if err == nil {
		raw, err = p.call(ctx, "initialize", params)
	}
	if err == nil {
		var init struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		}
		if json.Unmarshal(raw, &init) != nil {
			err = errors.New("initialize result is not valid")
		} else {
			p.serverInfo = cut(strings.TrimSpace(init.ServerInfo.Name+" "+init.ServerInfo.Version), 128)
			err = p.notify(ctx, "notifications/initialized", nil, true)
		}
	}
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return p, nil
}

func (p *proc) hasExited() bool {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.exited
}

func (p *proc) exitReason() string {
	if v, ok := p.fatal.Load().(string); ok && v != "" {
		return v
	}
	why := "exited"
	if p.waitErr != nil {
		why = p.waitErr.Error()
	}
	if line, ok := p.lastStderr.Load().(string); ok && line != "" {
		why += "; last stderr line: " + cut(line, 200)
	}
	return why
}

// stop closes the server's stdin, then sends SIGTERM and SIGKILL to its
// process group, StopGrace apart, and waits for it to exit.
func (p *proc) stop() {
	p.stopOnce.Do(func() {
		p.stopping.Store(true)
		_ = p.stdin.Close()
		select {
		case <-p.done:
			return
		case <-time.After(StopGrace):
		}
		p.signal(syscall.SIGTERM)
		select {
		case <-p.done:
			return
		case <-time.After(StopGrace):
		}
		p.signal(syscall.SIGKILL)
	})
	<-p.done
}

// signal sends sig (SIGTERM or SIGKILL) to the server's process tree while
// its leader has not been reaped. On Windows both end the tree.
func (p *proc) signal(sig syscall.Signal) {
	p.gmu.Lock()
	defer p.gmu.Unlock()
	if p.reaped {
		return
	}
	if sig == syscall.SIGKILL {
		_ = p.tree.Kill()
	} else {
		_ = p.tree.Terminate()
	}
}

// kill ends the server at once, for a protocol violation.
func (p *proc) kill(why string) {
	p.fatal.CompareAndSwap(nil, why)
	slog.Warn("Killing MCP server", "server", p.s.name, "why", why)
	p.signal(syscall.SIGKILL)
}

func (p *proc) failAll(err *Error) {
	p.pmu.Lock()
	pending := p.pending
	p.pending = map[int64]chan rpcMessage{}
	p.exited = true
	p.pmu.Unlock()
	for _, ch := range pending {
		ch <- rpcMessage{local: err}
	}
}

// outMsg is one line for the server's stdin. done, when set, gets the
// write's result.
type outMsg struct {
	data []byte
	done chan error
}

var errExited = errors.New("the server exited")

// writer is the only goroutine that writes to the server's stdin. A write
// that does not finish in writeLimit ends the server.
func (p *proc) writer() {
	for {
		select {
		case <-p.done:
			return
		case m := <-p.out:
			_ = p.stdin.SetWriteDeadline(time.Now().Add(writeLimit))
			_, err := p.stdin.Write(m.data)
			if m.done != nil {
				m.done <- err
			}
			if err != nil && !p.stopping.Load() {
				p.kill("could not write to its stdin: " + err.Error())
			}
		}
	}
}

// send queues v for the server. With wait it waits for the write, or for
// ctx; without, a full queue ends the server, which is not reading its
// stdin.
func (p *proc) send(ctx context.Context, v any, wait bool) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m := outMsg{data: append(data, '\n')}
	if !wait {
		select {
		case p.out <- m:
			return nil
		case <-p.done:
			return errExited
		default:
			p.kill(fmt.Sprintf("more than %d messages waited for its stdin", writeQueue))
			return errExited
		}
	}
	m.done = make(chan error, 1)
	select {
	case p.out <- m:
	case <-p.done:
		return errExited
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-m.done:
		return err
	case <-p.done:
		return errExited
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rpcOut is a message the client writes.
type rpcOut struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Method  string    `json:"method,omitempty"`
	Params  any       `json:"params,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

func (p *proc) notify(ctx context.Context, method string, params any, wait bool) error {
	return p.send(ctx, rpcOut{JSONRPC: jsonrpcVersion, Method: method, Params: params}, wait)
}

// call sends one request and waits for its answer. When ctx ends first it
// tells the server with notifications/cancelled.
func (p *proc) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	p.pmu.Lock()
	if p.exited {
		p.pmu.Unlock()
		return nil, fail(CodeFailed, "MCP server %s exited; the next request starts it again", p.s.name)
	}
	p.pending[id] = ch
	p.pmu.Unlock()
	msg := rpcOut{JSONRPC: jsonrpcVersion, ID: id, Method: method}
	if params != nil {
		msg.Params = params
	}
	if err := p.send(ctx, msg, true); err != nil {
		p.forget(id)
		if ctx.Err() != nil {
			return nil, ctxError(ctx, p.s.name)
		}
		return nil, fail(CodeFailed, "could not send the request to MCP server %s: %v", p.s.name, err)
	}
	select {
	case m := <-ch:
		if m.local != nil {
			return nil, m.local
		}
		if m.Error != nil {
			return nil, fail(CodeFailed, "MCP server %s answered %s with error %d: %s", p.s.name, method, m.Error.Code,
				cut(m.Error.Message, 1000))
		}
		if len(m.Result) == 0 {
			return nil, fail(CodeFailed, "MCP server %s answered %s without a result", p.s.name, method)
		}
		return m.Result, nil
	case <-ctx.Done():
		if p.forget(id) {
			// Without waiting, so ctx being done does not keep it from the queue.
			_ = p.notify(ctx, "notifications/cancelled",
				map[string]any{"requestId": id, "reason": "cancelled by the agent"}, false)
		}
		return nil, ctxError(ctx, p.s.name)
	}
}

func (p *proc) forget(id int64) bool {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	_, ok := p.pending[id]
	delete(p.pending, id)
	return ok
}

func (p *proc) deliver(id int64, m rpcMessage) {
	p.pmu.Lock()
	ch, ok := p.pending[id]
	delete(p.pending, id)
	p.pmu.Unlock()
	if ok {
		ch <- m
	}
}

// The id of an answer too large to read: first, as Go and Python SDKs
// write it, or last, as the TypeScript SDK does.
var (
	idHead = regexp.MustCompile(`^\s*\{\s*(?:"jsonrpc"\s*:\s*"2\.0"\s*,\s*)?"id"\s*:\s*(\d{1,18})\s*,`)
	idTail = regexp.MustCompile(`,\s*"id"\s*:\s*(\d{1,18})\s*\}\s*$`)
)

func oversizedID(head, tail []byte) (int64, bool) {
	for _, m := range [][][]byte{idTail.FindSubmatch(tail), idHead.FindSubmatch(head)} {
		if m != nil {
			if id, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
				return id, true
			}
		}
	}
	return 0, false
}

func (p *proc) readStdout(r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	limit := p.s.m.opts.MaxMessage
	for {
		line, long, err := readLine(br, limit)
		if errors.Is(err, errLineTooLong) {
			if id, ok := oversizedID(long.head, long.tail); ok {
				p.deliver(id, rpcMessage{local: fail(CodeTooLarge, "MCP server %s sent an answer over %d bytes",
					p.s.name, limit)})
				continue
			}
			p.kill(fmt.Sprintf("sent a message over %d bytes", limit))
			_, _ = io.Copy(io.Discard, br)
			return
		}
		if err != nil {
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		p.handle(line)
	}
}

func (p *proc) handle(line []byte) {
	var m rpcMessage
	if json.Unmarshal(line, &m) != nil {
		slog.Warn("MCP server sent a line that is not JSON-RPC", "server", p.s.name, "line", cut(string(line), 200))
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		p.answer(m)
	case m.Method != "":
		switch m.Method {
		case "notifications/tools/list_changed":
			p.s.toolsChanged(p)
		case "notifications/message":
			slog.Debug("MCP server log", "server", p.s.name, "params", cut(string(m.Params), maxStderr))
		}
	case hasID:
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return
		}
		p.deliver(id, m)
	}
}

// answer replies to a request from the server. The client offers no
// capabilities, so only ping succeeds.
func (p *proc) answer(m rpcMessage) {
	reply := rpcOut{JSONRPC: jsonrpcVersion, ID: m.ID}
	if m.Method == "ping" {
		reply.Result = struct{}{}
	} else {
		reply.Error = &rpcError{Code: -32601, Message: "talaria does not serve " + cut(m.Method, 64)}
	}
	_ = p.send(context.Background(), reply, false)
}

func (p *proc) readStderr(r io.Reader) {
	br := bufio.NewReaderSize(r, 16*1024)
	var window time.Time
	lines, dropped := 0, 0
	defer func() {
		if dropped > 0 {
			slog.Warn("MCP server stderr lines not logged", "server", p.s.name, "lines", dropped)
		}
	}()
	for {
		line, long, err := readLine(br, maxStderr)
		if errors.Is(err, errLineTooLong) {
			line, err = long.head, nil
		}
		if err != nil {
			return
		}
		text := strings.ToValidUTF8(strings.TrimRight(string(line), "\r\n"), "�")
		if text == "" {
			continue
		}
		p.lastStderr.Store(text)
		if now := time.Now(); now.Sub(window) >= time.Second {
			if dropped > 0 {
				slog.Warn("MCP server stderr lines not logged", "server", p.s.name, "lines", dropped)
			}
			window, lines, dropped = now, 0, 0
		}
		if lines++; lines > stderrBurst {
			dropped++
			continue
		}
		slog.Info("MCP server stderr", "server", p.s.name, "line", text)
	}
}

var errLineTooLong = errors.New("line too long")

// longLine is the start and the end of a line that readLine did not keep.
type longLine struct{ head, tail []byte }

const keepEnds = 512

// readLine reads one line of at most limit bytes. For a longer line it
// discards it and returns errLineTooLong with its first and last bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, longLine, error) {
	var buf []byte
	var long longLine
	over := false
	for {
		frag, err := br.ReadSlice('\n')
		if !over && len(buf)+len(frag) > limit {
			over = true
			buf = append(buf, frag...)
			long.head = slices.Clone(buf[:min(len(buf), keepEnds)])
			buf = nil
		}
		if over {
			long.tail = append(long.tail, frag...)
			if n := len(long.tail); n > keepEnds {
				long.tail = slices.Clone(long.tail[n-keepEnds:])
			}
		} else {
			buf = append(buf, frag...)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case over && (err == nil || errors.Is(err, io.EOF)):
			long.tail = bytes.TrimRight(long.tail, "\r\n")
			return nil, long, errLineTooLong
		case err == nil:
			return buf, longLine{}, nil
		case len(buf) > 0 && errors.Is(err, io.EOF):
			return buf, longLine{}, nil
		default:
			return nil, longLine{}, err
		}
	}
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}
