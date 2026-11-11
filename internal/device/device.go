// Package device is the exec leg client of the run daemon: it keeps one
// WebSocket to the hub's exec leg open and runs the commands the device
// plugin sends (docs/protocol.md section 4).
package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/executor"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

// Errors that stop Run. hubapi.ErrUnauthorized also stops it.
var (
	ErrReplaced           = errors.New("another talaria run with the same token took over this device (close 4409); stop the other daemon or run login again on one of them")
	ErrUnsupportedVersion = errors.New("the Hermes device plugin and this talaria share no protocol version (close 4426); upgrade talaria and the plugin together")
	ErrWindows            = errors.New("talaria does not support Windows: device paths must be POSIX")
	ErrUnsupportedOS      = errors.New("the Hermes device plugin does not serve this device's operating system (close 4427)")
	ErrNoWelcome          = errors.New("no welcome from the device plugin")
	ErrNoResume           = errors.New("no resume from the device plugin after a version 3 welcome")
)

// SilenceTimeout is how long the exec leg may go without any message from
// the plugin before the device drops it.
const SilenceTimeout = 60 * time.Second

// StableAfter is how long a connection must stay up to reset the backoff.
const StableAfter = 30 * time.Second

// Resume defaults.
const (
	DefaultResumeBuffer = 1 << 20
	DefaultMaxGrace     = 10 * time.Minute
)

const (
	writeTimeout = 30 * time.Second
	dialTimeout  = 30 * time.Second
)

// Dialer opens the exec leg. *hubapi.Client is one.
type Dialer interface {
	DialLeg(ctx context.Context, path string) (*websocket.Conn, error)
}

// Browser runs browser operations in the user's browser. *broker.Broker is
// one.
type Browser interface {
	Call(ctx context.Context, op string, args any) (json.RawMessage, error)
	Connected() (broker.Hello, bool)
	// Changed is closed the next time the extension connects or goes away.
	Changed() <-chan struct{}
}

// BrowserOps are the operations the device passes to the browser
// extension. Anything else is refused on the device.
var BrowserOps = map[string]bool{ //nolint:gochecknoglobals // fixed table
	"tabs_list": true, "tab_open": true, "tab_close": true, "navigate": true, "back": true,
	"read_text": true, "snapshot": true, "inspect": true, "click": true, "type": true, "screenshot": true,
	"press": true, "hover": true, "scroll": true, "wait": true, "eval": true, "login": true, "check": true,
}

// Options configure a Client. Zero durations take the protocol defaults.
type Options struct {
	Hub   Dialer
	Allow *allowlist.List
	// Browser serves browser requests. With it the device offers
	// protocol.CapabilityBrowser in hello.
	Browser Browser
	// MCP serves MCP requests for the servers of the device config. With
	// servers the device offers protocol.CapabilityMCP in hello.
	MCP MCP
	// StartBrowser starts the user's Firefox for a browser_start, when
	// the extension is not connected. Nil turns browser_start off.
	StartBrowser func(ctx context.Context) error
	// Shell runs each command as <shell> -c <cmd>.
	Shell string
	// TempDir is sent as hello.tmp_dir.
	TempDir string
	// Home is sent as hello.home. Defaults to $HOME.
	Home string
	// Hostname defaults to os.Hostname.
	Hostname      string
	ClientVersion string
	// Env is the environment of every command. Defaults to os.Environ.
	Env        []string
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// PingInterval is the interval of both the WebSocket control pings and
	// the application pings.
	PingInterval   time.Duration
	PingTimeout    time.Duration
	HelloTimeout   time.Duration
	SilenceTimeout time.Duration
	StableAfter    time.Duration
	// ResumeBuffer is the most output bytes per command that the device
	// keeps for a resume (version 3) until the plugin confirms them.
	ResumeBuffer int
	// MaxGrace caps the plugin's grace_seconds: the longest the device
	// keeps commands running after the exec leg drops.
	MaxGrace time.Duration
	// MaxVersion is the newest protocol version to offer. Defaults to
	// protocol.Version. Tests use it to act as an older client.
	MaxVersion int
	// OnWelcome, when set, is called with each accepted welcome, before
	// the leg serves requests. It must not block.
	OnWelcome func(protocol.Welcome)
}

// Client is the device side of the exec leg. Commands belong to the
// client, not to one connection, so that a version 3 leg can resume them
// after a reconnect (docs/protocol.md section 4.7).
type Client struct {
	opts Options

	// startMu guards lastStart, the time of the last Firefox start.
	startMu   sync.Mutex
	lastStart time.Time

	// seq numbers every command message written, across connections.
	// acked is the highest seq the plugin confirmed by answering a ping
	// written after it.
	seq   atomic.Uint64
	acked atomic.Uint64
	sweep chan struct{}
	wg    sync.WaitGroup

	mu        sync.Mutex
	cmds      map[string]*command
	parkGen   uint64
	parkTimer *time.Timer

	// calls counts browser operations and MCP calls in flight. draining
	// refuses new work once Drain found the device idle.
	calls    atomic.Int64
	draining atomic.Bool
}

// drainingMessage ends the refusal of work that arrives after Drain.
const drainingMessage = "is restarting for an update; try again in a few seconds"

// Busy reports whether the device holds a command (running, or with
// output or an exit the plugin has not confirmed), or runs a browser
// operation or an MCP call.
func (c *Client) Busy() bool {
	c.mu.Lock()
	n := len(c.cmds)
	c.mu.Unlock()
	return n > 0 || c.calls.Load() > 0
}

// Drain reports whether the device is idle and, when it is, refuses all
// new commands, browser operations and MCP calls from then on, so that the
// daemon can stop without cutting off work. When it returns false nothing
// changes.
func (c *Client) Drain() bool {
	if c.Busy() {
		return false
	}
	c.draining.Store(true)
	if c.Busy() {
		c.draining.Store(false)
		return false
	}
	return true
}

// beginCall counts a browser operation or MCP call in flight. It returns
// false, counting nothing, once the device drains.
func (c *Client) beginCall() bool {
	c.calls.Add(1)
	if c.draining.Load() {
		c.calls.Add(-1)
		return false
	}
	return true
}

func (c *Client) endCall() { c.calls.Add(-1) }

// New returns a client. It refuses to run on Windows.
func New(opts Options) (*Client, error) {
	if runtime.GOOS == "windows" {
		return nil, ErrWindows
	}
	if opts.Hub == nil || opts.Allow == nil {
		return nil, errors.New("device: Hub and Allow are required")
	}
	if opts.Shell == "" {
		opts.Shell = "bash"
	}
	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	if opts.Home == "" {
		opts.Home = os.Getenv("HOME")
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	setDefault(&opts.MinBackoff, time.Second)
	setDefault(&opts.MaxBackoff, 30*time.Second)
	setDefault(&opts.PingInterval, protocol.PingInterval)
	setDefault(&opts.PingTimeout, protocol.PingTimeout)
	setDefault(&opts.HelloTimeout, protocol.HelloTimeout)
	setDefault(&opts.SilenceTimeout, SilenceTimeout)
	setDefault(&opts.StableAfter, StableAfter)
	setDefault(&opts.MaxGrace, DefaultMaxGrace)
	if opts.ResumeBuffer <= 0 {
		opts.ResumeBuffer = DefaultResumeBuffer
	}
	if opts.MaxVersion < protocol.MinVersion || opts.MaxVersion > protocol.Version {
		opts.MaxVersion = protocol.Version
	}
	opts.MaxBackoff = max(opts.MaxBackoff, opts.MinBackoff)
	return &Client{opts: opts, cmds: map[string]*command{}, sweep: make(chan struct{}, 1)}, nil
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d <= 0 {
		*d = v
	}
}

// Hello returns the hello message the client sends.
func (c *Client) Hello() protocol.Hello {
	h := protocol.Hello{
		Type: protocol.TypeHello, Version: protocol.MinVersion, MaxVersion: c.opts.MaxVersion, Roots: c.opts.Allow.Roots(),
		TempDir: c.opts.TempDir, Home: c.opts.Home, Hostname: c.opts.Hostname, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Shell: c.opts.Shell, ClientVersion: c.opts.ClientVersion,
	}
	if c.opts.Browser != nil {
		h.Capabilities = append(h.Capabilities, protocol.CapabilityBrowser)
	}
	if c.opts.MCP != nil && len(c.opts.MCP.Names()) > 0 {
		h.Capabilities = append(h.Capabilities, protocol.CapabilityMCP)
		h.MCPServers = c.opts.MCP.Names()
	}
	return h
}

// Run connects and serves until ctx ends or the hub or plugin tells the
// device to stop: hubapi.ErrUnauthorized, ErrReplaced or
// ErrUnsupportedVersion. Every other drop is retried with backoff. Run
// kills every command it still holds before it returns.
func (c *Client) Run(ctx context.Context) error {
	sctx, stopSweep := context.WithCancel(ctx)
	c.wg.Go(func() { c.sweeper(sctx) })
	defer func() {
		stopSweep()
		c.killAll("the daemon stopped")
		c.wg.Wait()
	}()
	backoff := c.opts.MinBackoff
	for {
		up, err := c.serveOnce(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown is not an error
		}
		if isFatal(err) {
			return err
		}
		if up >= c.opts.StableAfter {
			backoff = c.opts.MinBackoff
		}
		slog.Warn("Exec leg lost; reconnecting", "error", err, "in", backoff)
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, c.opts.MaxBackoff)
	}
}

func isFatal(err error) bool {
	return errors.Is(err, hubapi.ErrUnauthorized) || errors.Is(err, ErrReplaced) || errors.Is(err, ErrUnsupportedVersion) ||
		errors.Is(err, ErrUnsupportedOS)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// stopError maps a close code from the hub or plugin that means "stop"
// (docs/protocol.md section 2.3) to its error, with the peer's reason, and
// returns nil for every other close.
func stopError(err error) error {
	var base error
	switch int(websocket.CloseStatus(err)) {
	case protocol.CloseUnauthorized:
		base = hubapi.ErrUnauthorized
	case protocol.CloseReplaced:
		base = ErrReplaced
	case protocol.CloseUnsupportedVersion:
		base = ErrUnsupportedVersion
	case protocol.CloseUnsupportedPlatform:
		base = ErrUnsupportedOS
	default:
		return nil
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) && ce.Reason != "" {
		return fmt.Errorf("%w: %s", base, ce.Reason)
	}
	return base
}

// resumable reports whether the commands of a version 3 leg that ended
// with err may wait for a resume. A gateway that stops (close 1012) loses
// its pending commands, so nothing would resume them.
func resumable(err error) bool {
	return int(websocket.CloseStatus(err)) != protocol.CloseServiceRestart && stopError(err) == nil
}

type session struct {
	c        *Client
	conn     *websocket.Conn
	ctx      context.Context //nolint:containedctx // the connection's lifetime, for writes from command goroutines
	cancel   context.CancelFunc
	name     string
	version  int
	lastSeen atomic.Int64
	pingSeq  atomic.Int64
	wmu      sync.Mutex
	pmu      sync.Mutex
	pings    map[int64]uint64
	wg       sync.WaitGroup
	// browser is true when the leg negotiated protocol.CapabilityBrowser.
	// calls holds the cancel function of each browser request in flight.
	browser bool
	bmu     sync.Mutex
	calls   map[string]context.CancelFunc
	// starts holds the cancel function of each browser_start in flight.
	starts map[string]context.CancelFunc
	// mcp is true when the leg negotiated protocol.CapabilityMCP.
	// mcpCalls holds the cancel function of each MCP request in flight.
	mcp      bool
	mcpCalls map[string]context.CancelFunc
}

func (s *session) write(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
}

// sendRaw writes an encoded message.
func (s *session) sendRaw(ctx context.Context, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
}

func (s *session) send(ctx context.Context, v any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.write(ctx, v)
}

// sendCmd writes an output or exit message and returns its seq, which is
// 0 when the write failed.
func (s *session) sendCmd(ctx context.Context, v any) uint64 {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	seq := s.c.seq.Add(1)
	if s.write(ctx, v) != nil {
		return 0
	}
	return seq
}

// ping writes an application ping and remembers which command messages
// were written before it: the plugin's pong confirms them, because the hub
// relays in order and the plugin handles messages in order. The mark is
// stored before the write, because the pong can arrive before the write
// returns.
func (s *session) ping(ctx context.Context) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	n := s.pingSeq.Add(1)
	s.pmu.Lock()
	s.pings[n] = s.c.seq.Load()
	s.pmu.Unlock()
	if s.write(ctx, protocol.Ping{Type: protocol.TypePing, Seq: n}) != nil {
		s.pmu.Lock()
		delete(s.pings, n)
		s.pmu.Unlock()
	}
}

func (s *session) pong(n int64) {
	s.pmu.Lock()
	mark, ok := s.pings[n]
	for k := range s.pings {
		if k <= n {
			delete(s.pings, k)
		}
	}
	s.pmu.Unlock()
	if !ok {
		return
	}
	for {
		old := s.c.acked.Load()
		if mark <= old || s.c.acked.CompareAndSwap(old, mark) {
			break
		}
	}
	select {
	case s.c.sweep <- struct{}{}:
	default:
	}
}

func (s *session) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

// serveOnce runs one connection. It returns how long the connection was
// up after the welcome, and why it ended.
func (c *Client) serveOnce(ctx context.Context) (time.Duration, error) {
	dctx, dcancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := c.opts.Hub.DialLeg(dctx, hubapi.PathExecLeg)
	dcancel()
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.CloseNow() }()

	connCtx, cancel := context.WithCancel(ctx)
	s := &session{c: c, conn: conn, ctx: connCtx, cancel: cancel, pings: map[int64]uint64{},
		calls: map[string]context.CancelFunc{}, mcpCalls: map[string]context.CancelFunc{}}
	defer func() {
		cancel()
		s.wg.Wait()
	}()

	if err := s.send(connCtx, c.Hello()); err != nil {
		return 0, fmt.Errorf("hello: %w", err)
	}
	welcome, err := s.readWelcome(connCtx)
	if err != nil {
		if !resumable(err) {
			c.killAll("the exec leg closed for good")
		}
		return 0, err
	}
	if welcome.Version < protocol.MinVersion || welcome.Version > c.opts.MaxVersion {
		reason := fmt.Sprintf("talaria %s speaks protocol %d to %d; plugin %s chose %d",
			c.opts.ClientVersion, protocol.MinVersion, c.opts.MaxVersion, cut(welcome.PluginVersion, 16), welcome.Version)
		_ = conn.Close(protocol.CloseUnsupportedVersion, reason)
		return 0, fmt.Errorf("%w: %s", ErrUnsupportedVersion, reason)
	}
	if c.opts.OnWelcome != nil {
		c.opts.OnWelcome(*welcome)
	}
	s.version = welcome.Version
	s.name = welcome.Name
	if s.name == "" {
		s.name = c.opts.Hostname
	}
	grace := min(time.Duration(welcome.GraceSeconds*float64(time.Second)), c.opts.MaxGrace)
	s.touch()
	if s.version >= protocol.ResumeVersion {
		r, err := s.readResume(connCtx)
		if err != nil {
			if !resumable(err) {
				c.killAll("the exec leg closed for good")
			}
			return 0, err
		}
		c.resume(connCtx, s, r)
	} else {
		c.killAll("the plugin speaks protocol 2, which cannot resume commands")
	}
	s.browser = c.opts.Browser != nil && slices.Contains(welcome.Capabilities, protocol.CapabilityBrowser)
	s.mcp = c.opts.MCP != nil && slices.Contains(c.Hello().Capabilities, protocol.CapabilityMCP) &&
		slices.Contains(welcome.Capabilities, protocol.CapabilityMCP)
	connected := time.Now()
	slog.Info("Exec leg connected", "device_id", welcome.DeviceID, "name", welcome.Name, "protocol", s.version,
		"plugin_version", welcome.PluginVersion, "hermes_version", welcome.HermesVersion, "hermes_image", welcome.HermesImage,
		"roots", c.opts.Allow.Roots(),
		"browser", s.browser, "mcp", s.mcp)

	if s.browser {
		s.wg.Go(func() { s.browserStatus(connCtx) })
	}
	s.wg.Go(func() { s.controlPings(connCtx) })
	s.wg.Go(func() { s.appPings(connCtx) })
	err = s.readLoop(connCtx)
	up := time.Since(connected)
	_ = conn.CloseNow()
	cancel()
	s.wg.Wait()
	switch {
	case ctx.Err() != nil:
	case s.version >= protocol.ResumeVersion && grace > 0 && resumable(err):
		c.park(s, grace)
	default:
		c.killAll("the exec leg closed and the plugin will not resume its commands")
	}
	return up, err
}

func (s *session) readWelcome(ctx context.Context) (*protocol.Welcome, error) {
	hctx, cancel := context.WithTimeout(ctx, s.c.opts.HelloTimeout)
	defer cancel()
	for {
		_, data, err := s.conn.Read(hctx)
		if err != nil {
			if se := stopError(err); se != nil {
				return nil, se
			}
			if hctx.Err() != nil && ctx.Err() == nil {
				return nil, ErrNoWelcome
			}
			return nil, fmt.Errorf("welcome: %w", err)
		}
		var env protocol.Envelope
		if json.Unmarshal(data, &env) != nil || env.Type != protocol.TypeWelcome {
			slog.Debug("Ignoring message before welcome", "type", env.Type)
			continue
		}
		var w protocol.Welcome
		if err := json.Unmarshal(data, &w); err != nil {
			return nil, fmt.Errorf("welcome: %w", err)
		}
		return &w, nil
	}
}

// readResume reads the resume that a version 3 plugin sends right after
// its welcome. Pings before it are answered.
func (s *session) readResume(ctx context.Context) (*protocol.Resume, error) {
	hctx, cancel := context.WithTimeout(ctx, s.c.opts.HelloTimeout)
	defer cancel()
	for {
		_, data, err := s.conn.Read(hctx)
		if err != nil {
			if se := stopError(err); se != nil {
				return nil, se
			}
			if hctx.Err() != nil && ctx.Err() == nil {
				_ = s.conn.Close(websocket.StatusProtocolError, "expected resume")
				return nil, ErrNoResume
			}
			return nil, fmt.Errorf("resume: %w", err)
		}
		var env protocol.Envelope
		_ = json.Unmarshal(data, &env)
		switch env.Type {
		case protocol.TypeResume:
			var r protocol.Resume
			if err := json.Unmarshal(data, &r); err != nil {
				_ = s.conn.Close(websocket.StatusProtocolError, "malformed resume")
				return nil, fmt.Errorf("resume: %w", err)
			}
			return &r, nil
		case protocol.TypePing:
			var p protocol.Ping
			if json.Unmarshal(data, &p) == nil {
				_ = s.send(ctx, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
			}
		default:
			_ = s.conn.Close(websocket.StatusProtocolError, "expected resume")
			return nil, fmt.Errorf("%w: got %q", ErrNoResume, env.Type)
		}
	}
}

func (s *session) readLoop(ctx context.Context) error {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			if se := stopError(err); se != nil {
				return se
			}
			return fmt.Errorf("read: %w", err)
		}
		s.touch()
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			slog.Warn("Ignoring malformed message", "error", err)
			continue
		}
		switch env.Type {
		case protocol.TypeExec:
			var req protocol.Exec
			if err := json.Unmarshal(data, &req); err != nil {
				slog.Warn("Ignoring malformed exec", "error", err)
				continue
			}
			s.c.exec(ctx, s, req)
		case protocol.TypeKill:
			var req protocol.Kill
			if err := json.Unmarshal(data, &req); err == nil {
				s.c.kill(req.ID)
				s.cancelBrowser(req.ID)
				s.cancelMCP(req.ID)
			}
		case protocol.TypeBrowser:
			if !s.browser {
				slog.Debug("Ignoring browser request on a leg without the browser capability")
				continue
			}
			var req protocol.Browser
			if err := json.Unmarshal(data, &req); err != nil {
				slog.Warn("Ignoring malformed browser request", "error", err)
				continue
			}
			s.browserCall(ctx, req)
		case protocol.TypeMCP:
			s.mcpRequest(ctx, data)
		case protocol.TypeBrowserStart:
			if !s.browser {
				continue
			}
			var req protocol.BrowserStart
			if err := json.Unmarshal(data, &req); err != nil {
				slog.Warn("Ignoring malformed browser_start", "error", err)
				continue
			}
			s.browserStart(ctx, req)
		case protocol.TypePing:
			var p protocol.Ping
			if err := json.Unmarshal(data, &p); err == nil {
				_ = s.send(ctx, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
			}
		case protocol.TypePong:
			var p protocol.Ping
			if err := json.Unmarshal(data, &p); err == nil {
				s.pong(p.Seq)
			}
		default:
			slog.Debug("Ignoring message", "type", env.Type)
		}
	}
}

// controlPings sends WebSocket ping frames and drops the hop when a pong
// does not come back within PingTimeout.
func (s *session) controlPings(ctx context.Context) {
	t := time.NewTicker(s.c.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, s.c.opts.PingTimeout)
			err := s.conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("WebSocket ping failed; dropping the exec leg", "error", err)
				s.drop("ping timeout")
				return
			}
		}
	}
}

// appPings sends application pings through the hub to the plugin and
// drops the connection after SilenceTimeout without any message.
func (s *session) appPings(ctx context.Context) {
	pingT := time.NewTicker(s.c.opts.PingInterval)
	defer pingT.Stop()
	checkT := time.NewTicker(max(s.c.opts.SilenceTimeout/8, time.Millisecond))
	defer checkT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingT.C:
			s.ping(ctx)
		case now := <-checkT.C:
			if silent := now.Sub(time.Unix(0, s.lastSeen.Load())); silent >= s.c.opts.SilenceTimeout {
				slog.Warn("No message from the device plugin; dropping the exec leg", "silent", silent.Round(time.Millisecond))
				s.drop("no message")
				return
			}
		}
	}
}

// drop sends a ClosePingTimeout close frame and tears the connection down
// within a second, whether or not the peer answers.
func (s *session) drop(reason string) {
	done := make(chan struct{})
	s.wg.Go(func() {
		_ = s.conn.Close(protocol.ClosePingTimeout, reason)
		close(done)
	})
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	s.cancel()
}

// command is one command the plugin started, running or finished. A
// finished command stays until the plugin confirms its exit, so that a
// resume can send the exit again.
type command struct {
	id  string
	max int

	mu      sync.Mutex
	proc    *executor.Process
	link    *session
	partial []byte
	next    int64
	chunks  []chunk
	size    int
	exit    *protocol.Exit
	exitSeq uint64
}

// chunk is one output message. seq is 0 until it was written.
type chunk struct {
	off  int64
	data string
	seq  uint64
}

func (cmd *command) running() bool {
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	return cmd.exit == nil && cmd.proc != nil
}

func (cmd *command) onOutput(b []byte) {
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	cmd.partial = append(cmd.partial, b...)
	cut := completeUTF8(cmd.partial)
	if cut == 0 {
		return
	}
	cmd.emit(cmd.partial[:cut])
	cmd.partial = append(cmd.partial[:0], cmd.partial[cut:]...)
}

// emit buffers and sends one chunk. Call with mu held.
func (cmd *command) emit(b []byte) {
	ch := chunk{off: cmd.next, data: validUTF8(b)}
	cmd.next += int64(len(ch.data))
	cmd.chunks = append(cmd.chunks, ch)
	cmd.size += len(ch.data)
	for cmd.size > cmd.max && len(cmd.chunks) > 1 {
		cmd.size -= len(cmd.chunks[0].data)
		cmd.chunks = cmd.chunks[1:]
	}
	if cmd.size > cmd.max {
		cmd.keepTail()
	}
	if cmd.link != nil {
		cmd.chunks[len(cmd.chunks)-1].seq = cmd.link.sendCmd(cmd.link.ctx, protocol.Output{
			Type: protocol.TypeOutput, ID: cmd.id, Data: ch.data, Offset: ch.off,
		})
	}
}

// keepTail cuts the only buffered chunk to its last max bytes, on a rune
// boundary, so one large write cannot exceed the resume buffer. Call with mu held.
func (cmd *command) keepTail() {
	ch := &cmd.chunks[0]
	cut := len(ch.data) - cmd.max
	for cut < len(ch.data) && !utf8.RuneStart(ch.data[cut]) {
		cut++
	}
	ch.off += int64(cut)
	ch.data = ch.data[cut:]
	cmd.size = len(ch.data)
}

// finish records the exit and sends it. It reports whether the command
// was linked to a version 2 leg, which never resumes it.
func (cmd *command) finish(exit protocol.Exit) bool {
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	if len(cmd.partial) > 0 {
		cmd.emit(cmd.partial)
		cmd.partial = nil
	}
	cmd.exit = &exit
	if cmd.link != nil {
		cmd.exitSeq = cmd.link.sendCmd(cmd.link.ctx, exit)
		return cmd.link.version < protocol.ResumeVersion
	}
	return false
}

// trim drops chunks the plugin confirmed and reports whether the plugin
// also confirmed the exit.
func (cmd *command) trim(acked uint64) bool {
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	i := 0
	for i < len(cmd.chunks) && cmd.chunks[i].seq != 0 && cmd.chunks[i].seq <= acked {
		cmd.size -= len(cmd.chunks[i].data)
		i++
	}
	cmd.chunks = cmd.chunks[i:]
	return cmd.exit != nil && cmd.exitSeq != 0 && cmd.exitSeq <= acked
}

// attach links the command to s and sends the output from offset on, and
// the exit when the command has ended.
func (cmd *command) attach(s *session, offset int64) {
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	cmd.link = s
	i := 0
	for i < len(cmd.chunks) && cmd.chunks[i].off+int64(len(cmd.chunks[i].data)) <= offset {
		cmd.size -= len(cmd.chunks[i].data)
		i++
	}
	cmd.chunks = cmd.chunks[i:]
	for i := range cmd.chunks {
		ch := &cmd.chunks[i]
		ch.seq = s.sendCmd(s.ctx, protocol.Output{Type: protocol.TypeOutput, ID: cmd.id, Data: ch.data, Offset: ch.off})
	}
	if cmd.exit != nil {
		cmd.exitSeq = s.sendCmd(s.ctx, *cmd.exit)
	}
}

func (cmd *command) unlink(s *session) {
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	if cmd.link == s {
		cmd.link = nil
	}
}

func (cmd *command) kill() {
	cmd.mu.Lock()
	p, done := cmd.proc, cmd.exit != nil
	cmd.mu.Unlock()
	if p != nil && !done {
		slog.Info("Killing command", "id", cmd.id, "pid", p.Pid())
		p.Kill()
	}
}

// cut shortens s to at most n bytes, on a character boundary, so close
// reasons stay under the hub's 120-byte cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// validUTF8 replaces each invalid byte with U+FFFD, as encoding/json would,
// so that offsets count the bytes the plugin receives.
func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.Write(b[:n])
		}
		b = b[n:]
	}
	return sb.String()
}

func (c *Client) remove(cmd *command) {
	c.mu.Lock()
	if c.cmds[cmd.id] == cmd {
		delete(c.cmds, cmd.id)
	}
	c.mu.Unlock()
}

func (c *Client) snapshot() []*command {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*command, 0, len(c.cmds))
	for _, cmd := range c.cmds {
		out = append(out, cmd)
	}
	return out
}

// sweeper drops what the plugin confirmed, after each pong.
func (c *Client) sweeper(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.sweep:
		}
		acked := c.acked.Load()
		for _, cmd := range c.snapshot() {
			if cmd.trim(acked) {
				c.remove(cmd)
			}
		}
	}
}

// takeAll forgets every command and stops the grace timer.
func (c *Client) takeAll() map[string]*command {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.takeAllLocked()
}

func (c *Client) takeAllLocked() map[string]*command {
	cmds := c.cmds
	c.cmds = map[string]*command{}
	c.parkGen++
	if c.parkTimer != nil {
		c.parkTimer.Stop()
		c.parkTimer = nil
	}
	return cmds
}

func killEach(cmds map[string]*command, why string) {
	n := 0
	for _, cmd := range cmds {
		if cmd.running() {
			n++
		}
		cmd.kill()
	}
	if n > 0 {
		slog.Warn("Killing commands", "count", n, "why", why)
	}
}

// killAll kills every running command and forgets every command.
func (c *Client) killAll(why string) { killEach(c.takeAll(), why) }

// park keeps the commands of the dropped leg s running for grace, waiting
// for a resume on the next connection.
func (c *Client) park(s *session, grace time.Duration) {
	cmds := c.snapshot()
	for _, cmd := range cmds {
		cmd.unlink(s)
	}
	c.mu.Lock()
	c.parkGen++
	gen := c.parkGen
	if c.parkTimer != nil {
		c.parkTimer.Stop()
	}
	c.parkTimer = time.AfterFunc(grace, func() { c.expire(gen) })
	c.mu.Unlock()
	if len(cmds) > 0 {
		slog.Info("Keeping commands for a resume", "count", len(cmds), "grace", grace)
	}
}

func (c *Client) expire(gen uint64) {
	c.mu.Lock()
	if gen != c.parkGen {
		c.mu.Unlock()
		return
	}
	cmds := c.takeAllLocked()
	c.mu.Unlock()
	killEach(cmds, "no resume within the grace period")
}

// resume answers the plugin's resume on s: it forgets every command
// whose exit the plugin confirmed, kills and forgets every command the
// plugin no longer waits for, and sends the rest their missing output and
// exit.
func (c *Client) resume(ctx context.Context, s *session, r *protocol.Resume) {
	want := make(map[string]int64, len(r.Commands))
	order := make([]string, 0, len(r.Commands))
	for _, rc := range r.Commands {
		if _, dup := want[rc.ID]; !dup {
			want[rc.ID] = rc.Offset
			order = append(order, rc.ID)
		}
	}
	held := c.takeAll()
	acked := c.acked.Load()
	for id, cmd := range held {
		if cmd.trim(acked) {
			delete(held, id)
		}
	}
	orphans := map[string]*command{}
	c.mu.Lock()
	for id, cmd := range held {
		if _, ok := want[id]; ok {
			c.cmds[id] = cmd
		} else {
			orphans[id] = cmd
		}
	}
	c.mu.Unlock()
	killEach(orphans, "the plugin no longer waits for them")
	for _, id := range order {
		cmd := held[id]
		if cmd == nil {
			s.sendCmd(ctx, protocol.Exit{
				Type: protocol.TypeExit, ID: id, Code: protocol.ExitLost,
				Error: fmt.Sprintf("device %s has no record of this command: its daemon restarted or the resume grace ran out", s.name),
			})
			continue
		}
		cmd.attach(s, want[id])
	}
	if len(order) > 0 {
		slog.Info("Resumed commands", "count", len(order))
	}
}

func (c *Client) kill(id string) {
	c.mu.Lock()
	cmd := c.cmds[id]
	c.mu.Unlock()
	if cmd != nil {
		cmd.kill()
	}
}

func (c *Client) finished(cmd *command, exit protocol.Exit) {
	if cmd.finish(exit) {
		c.remove(cmd)
	}
}

func (c *Client) exec(ctx context.Context, s *session, req protocol.Exec) {
	cmd := &command{id: req.ID, link: s, max: c.opts.ResumeBuffer}
	c.mu.Lock()
	draining := c.draining.Load()
	_, dup := c.cmds[req.ID]
	if !dup && !draining {
		c.cmds[req.ID] = cmd
	}
	c.mu.Unlock()
	if draining {
		s.sendCmd(ctx, protocol.Exit{
			Type: protocol.TypeExit, ID: req.ID, Code: protocol.ExitRefused,
			Error: fmt.Sprintf("device %s refused: it %s", s.name, drainingMessage),
		})
		return
	}
	if dup {
		s.sendCmd(ctx, protocol.Exit{
			Type: protocol.TypeExit, ID: req.ID, Code: protocol.ExitRefused,
			Error: fmt.Sprintf("device %s refused: request id %q is already running", s.name, req.ID),
		})
		return
	}
	refuse := func(msg string) {
		c.finished(cmd, protocol.Exit{Type: protocol.TypeExit, ID: req.ID, Code: protocol.ExitRefused, Error: msg})
	}
	cwd, err := c.opts.Allow.Check(req.Cwd)
	if err != nil {
		slog.Warn("Refusing command", "id", req.ID, "cwd", req.Cwd, "error", err)
		refuse(fmt.Sprintf("device %s refused: working directory %s", s.name, err))
		return
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	p, err := executor.Start(c.opts.Shell, executor.Request{
		Cmd: req.Cmd, Cwd: cwd, Login: req.Login, Timeout: timeout, Stdin: req.Stdin, Env: c.opts.Env,
	}, cmd.onOutput)
	if err != nil {
		refuse(fmt.Sprintf("device %s could not start the command: %s", s.name, err))
		return
	}
	slog.Debug("Started command", "id", req.ID, "pid", p.Pid(), "cwd", cwd, "login", req.Login)
	cmd.mu.Lock()
	cmd.proc = p
	cmd.mu.Unlock()
	c.wg.Go(func() {
		res, _ := p.Wait(context.WithoutCancel(ctx))
		c.finished(cmd, protocol.Exit{Type: protocol.TypeExit, ID: req.ID, Code: res.Code, Killed: res.Killed})
		slog.Debug("Command ended", "id", req.ID, "code", res.Code, "killed", res.Killed)
	})
}

// completeUTF8 returns the length of the longest prefix of b that does not
// end in the middle of a UTF-8 sequence. Invalid bytes count as complete;
// encoding/json replaces them with U+FFFD.
func completeUTF8(b []byte) int {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if utf8.FullRune(b[len(b)-i:]) {
				return len(b)
			}
			return len(b) - i
		}
	}
	return len(b)
}

// browserStatus sends a BrowserStatus now and each time the extension
// connects or goes away.
func (s *session) browserStatus(ctx context.Context) {
	b := s.c.opts.Browser
	var last protocol.BrowserStatus
	for {
		changed := b.Changed()
		st := protocol.BrowserStatus{Type: protocol.TypeBrowserStatus}
		if h, ok := b.Connected(); ok {
			st.Connected, st.Browser, st.ExtensionVersion = true, h.Browser, h.Version
		}
		st.CanStart = s.c.opts.StartBrowser != nil
		if st != last {
			if err := s.send(ctx, st); err != nil {
				return
			}
			last = st
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func (s *session) cancelBrowser(id string) {
	s.bmu.Lock()
	cancel := s.calls[id]
	if cancel == nil {
		cancel = s.starts[id]
	}
	s.bmu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// browserCall runs one browser request. Browser requests are never kept
// across a drop: the leg's context ends them, and their results go nowhere.
func (s *session) browserCall(ctx context.Context, req protocol.Browser) {
	fail := func(code, msg string) {
		_ = s.send(ctx, protocol.BrowserResult{Type: protocol.TypeBrowserResult, ID: req.ID, Code: code, Error: msg})
	}
	if !BrowserOps[req.Op] {
		fail(protocol.BrowserRefused, fmt.Sprintf("device %s refused: unknown browser operation %q", s.name, cut(req.Op, 32)))
		return
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = protocol.BrowserDefaultTimeout
	}
	timeout = min(timeout, protocol.BrowserMaxTimeout)
	if !s.c.beginCall() {
		fail(protocol.BrowserRefused, fmt.Sprintf("device %s refused: it %s", s.name, drainingMessage))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	s.bmu.Lock()
	_, dup := s.calls[req.ID]
	if !dup {
		s.calls[req.ID] = cancel
	}
	s.bmu.Unlock()
	if dup {
		cancel()
		s.c.endCall()
		fail(protocol.BrowserRefused, fmt.Sprintf("device %s refused: request id %q is already running", s.name, req.ID))
		return
	}
	args := req.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	s.wg.Go(func() {
		defer s.c.endCall()
		defer cancel()
		res, err := s.c.opts.Browser.Call(cctx, req.Op, args)
		s.bmu.Lock()
		delete(s.calls, req.ID)
		s.bmu.Unlock()
		out := protocol.BrowserResult{Type: protocol.TypeBrowserResult, ID: req.ID, OK: err == nil, Result: res}
		var extErr *broker.Error
		switch {
		case err == nil && len(res) > protocol.MaxBrowserResult:
			out = protocol.BrowserResult{Type: protocol.TypeBrowserResult, ID: req.ID, Code: protocol.BrowserFailed,
				Error: fmt.Sprintf("the %s result is %d bytes, over the %d byte limit", req.Op, len(res), protocol.MaxBrowserResult)}
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded):
			out.Code, out.Error = protocol.BrowserTimeout, fmt.Sprintf("browser operation %s timed out after %s", req.Op, timeout)
		case errors.Is(err, context.Canceled):
			out.Code, out.Error = protocol.BrowserCancelled, "browser operation cancelled"
		case errors.Is(err, broker.ErrNotConnected), errors.Is(err, broker.ErrClosed):
			out.Code, out.Error = protocol.BrowserNotConnected, err.Error()
		case errors.As(err, &extErr) && extErr.Refused():
			out.Code, out.Error = protocol.BrowserRefused, err.Error()
		default:
			out.Code, out.Error = protocol.BrowserFailed, err.Error()
		}
		_ = s.send(ctx, out)
	})
}

// startPoll is how often browserStart checks for the extension.
const startPoll = 500 * time.Millisecond

// StartCooldown is the least time between two Firefox starts.
const StartCooldown = time.Minute

// startCooldown reserves a Firefox start and returns 0, or returns how
// long to wait before the next one.
func (c *Client) startCooldown() time.Duration {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if since := time.Since(c.lastStart); !c.lastStart.IsZero() && since < StartCooldown {
		return StartCooldown - since
	}
	c.lastStart = time.Now()
	return 0
}

// browserStart starts Firefox when the extension is not connected and
// waits until it is connected (docs/browser.md section 5.1).
func (s *session) browserStart(ctx context.Context, req protocol.BrowserStart) {
	fail := func(code, msg string) {
		_ = s.send(ctx, protocol.BrowserResult{Type: protocol.TypeBrowserResult, ID: req.ID, Code: code, Error: msg})
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = protocol.BrowserDefaultTimeout
	}
	timeout = min(timeout, protocol.BrowserMaxTimeout)
	if !s.c.beginCall() {
		fail(protocol.BrowserRefused, fmt.Sprintf("device %s refused: it %s", s.name, drainingMessage))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	s.bmu.Lock()
	if s.starts == nil {
		s.starts = map[string]context.CancelFunc{}
	}
	_, dup := s.starts[req.ID]
	if !dup {
		s.starts[req.ID] = cancel
	}
	s.bmu.Unlock()
	if dup {
		cancel()
		s.c.endCall()
		fail(protocol.BrowserRefused, fmt.Sprintf("device %s refused: request id %q is already running", s.name, req.ID))
		return
	}
	s.wg.Go(func() {
		defer s.c.endCall()
		defer cancel()
		defer func() {
			s.bmu.Lock()
			delete(s.starts, req.ID)
			s.bmu.Unlock()
		}()
		res, code, msg := s.startAndWait(cctx, timeout)
		if code != "" {
			fail(code, msg)
			return
		}
		data, err := json.Marshal(res)
		if err != nil {
			fail(protocol.BrowserFailed, err.Error())
			return
		}
		_ = s.send(ctx, protocol.BrowserResult{Type: protocol.TypeBrowserResult, ID: req.ID, OK: true, Result: data})
	})
}

type startResult struct {
	Started bool `json:"started"`
}

func (s *session) startAndWait(ctx context.Context, timeout time.Duration) (startResult, string, string) {
	b := s.c.opts.Browser
	var res startResult
	if _, ok := b.Connected(); !ok {
		if s.c.opts.StartBrowser == nil {
			return res, protocol.BrowserRefused, fmt.Sprintf("device %s does not start Firefox (browser.start-firefox is "+
				"off); start Firefox on that computer", s.name)
		}
		if wait := s.c.startCooldown(); wait > 0 {
			return res, protocol.BrowserRefused, fmt.Sprintf("device %s started Firefox less than %s ago; wait %s",
				s.name, StartCooldown, wait.Round(time.Second))
		}
		slog.Info("Starting Firefox for the agent")
		if err := s.c.opts.StartBrowser(ctx); err != nil {
			slog.Warn("Could not start Firefox", "error", err)
			return res, protocol.BrowserFailed, fmt.Sprintf("device %s could not start Firefox: %v", s.name, err)
		}
		res.Started = true
	}
	tick := time.NewTicker(startPoll)
	defer tick.Stop()
	for {
		changed := b.Changed()
		if _, connected := b.Connected(); connected {
			return res, "", ""
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return res, protocol.BrowserCancelled, "browser start cancelled"
			}
			return res, protocol.BrowserTimeout, fmt.Sprintf("Firefox did not connect to talaria on %s within %s: "+
				"check that the Hermes extension is installed and enabled in the default Firefox profile", s.name, timeout)
		case <-changed:
		case <-tick.C:
		}
	}
}
