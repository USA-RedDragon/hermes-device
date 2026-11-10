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
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/executor"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

// Errors that stop Run. hubapi.ErrUnauthorized also stops it.
var (
	ErrReplaced           = errors.New("another hermes-device run with the same token took over this device (close 4409); stop the other daemon or run login again on one of them")
	ErrUnsupportedVersion = errors.New("the Hermes device plugin does not speak this protocol version (close 4426); upgrade hermes-device and the plugin together")
	ErrWindows            = errors.New("hermes-device does not support Windows: device paths must be POSIX")
	ErrUnsupportedOS      = errors.New("the Hermes device plugin does not serve this device's operating system (close 4427)")
	ErrNoWelcome          = errors.New("no welcome from the device plugin")
)

// SilenceTimeout is how long the exec leg may go without any message from
// the plugin before the device drops it.
const SilenceTimeout = 60 * time.Second

// StableAfter is how long a connection must stay up to reset the backoff.
const StableAfter = 30 * time.Second

const (
	writeTimeout = 30 * time.Second
	dialTimeout  = 30 * time.Second
)

// Dialer opens the exec leg. *hubapi.Client is one.
type Dialer interface {
	DialLeg(ctx context.Context, path string) (*websocket.Conn, error)
}

// Options configure a Client. Zero durations take the protocol defaults.
type Options struct {
	Hub   Dialer
	Allow *allowlist.List
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
}

// Client is the device side of the exec leg.
type Client struct {
	opts Options
}

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
	opts.MaxBackoff = max(opts.MaxBackoff, opts.MinBackoff)
	return &Client{opts: opts}, nil
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d <= 0 {
		*d = v
	}
}

// Hello returns the hello message the client sends.
func (c *Client) Hello() protocol.Hello {
	return protocol.Hello{
		Type: protocol.TypeHello, Version: protocol.Version, Roots: c.opts.Allow.Roots(), TempDir: c.opts.TempDir,
		Home: c.opts.Home, Hostname: c.opts.Hostname, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Shell: c.opts.Shell, ClientVersion: c.opts.ClientVersion,
	}
}

// Run connects and serves until ctx ends or the hub or plugin tells the
// device to stop: hubapi.ErrUnauthorized, ErrReplaced or
// ErrUnsupportedVersion. Every other drop is retried with backoff.
func (c *Client) Run(ctx context.Context) error {
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
// (docs/protocol.md section 2.3) to its error, and returns nil for every
// other close.
func stopError(err error) error {
	switch int(websocket.CloseStatus(err)) {
	case protocol.CloseUnauthorized:
		return hubapi.ErrUnauthorized
	case protocol.CloseReplaced:
		return ErrReplaced
	case protocol.CloseUnsupportedVersion:
		return ErrUnsupportedVersion
	case protocol.CloseUnsupportedPlatform:
		return ErrUnsupportedOS
	}
	return nil
}

type session struct {
	c        *Client
	conn     *websocket.Conn
	cancel   context.CancelFunc
	name     string
	lastSeen atomic.Int64
	seq      atomic.Int64
	procsMu  sync.Mutex
	procs    map[string]*executor.Process
	wg       sync.WaitGroup
}

func (s *session) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
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
	s := &session{c: c, conn: conn, cancel: cancel, procs: map[string]*executor.Process{}}
	defer func() {
		cancel()
		s.killAll()
		s.wg.Wait()
	}()

	if err := s.send(connCtx, c.Hello()); err != nil {
		return 0, fmt.Errorf("hello: %w", err)
	}
	welcome, err := s.readWelcome(connCtx)
	if err != nil {
		return 0, err
	}
	s.name = welcome.Name
	if s.name == "" {
		s.name = c.opts.Hostname
	}
	connected := time.Now()
	slog.Info("Exec leg connected", "device_id", welcome.DeviceID, "name", welcome.Name,
		"plugin_version", welcome.PluginVersion, "hermes_version", welcome.HermesVersion, "roots", c.opts.Allow.Roots())

	s.touch()
	s.wg.Go(func() { s.controlPings(connCtx) })
	s.wg.Go(func() { s.appPings(connCtx) })
	err = s.readLoop(connCtx)
	return time.Since(connected), err
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
			s.exec(ctx, req)
		case protocol.TypeKill:
			var req protocol.Kill
			if err := json.Unmarshal(data, &req); err == nil {
				s.kill(req.ID)
			}
		case protocol.TypePing:
			var p protocol.Ping
			if err := json.Unmarshal(data, &p); err == nil {
				_ = s.send(ctx, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
			}
		case protocol.TypePong:
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
			_ = s.send(ctx, protocol.Ping{Type: protocol.TypePing, Seq: s.seq.Add(1)})
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

func (s *session) kill(id string) {
	s.procsMu.Lock()
	p := s.procs[id]
	s.procsMu.Unlock()
	if p != nil {
		slog.Info("Killing command", "id", id, "pid", p.Pid())
		p.Kill()
	}
}

func (s *session) killAll() {
	s.procsMu.Lock()
	defer s.procsMu.Unlock()
	for _, p := range s.procs {
		p.Kill()
	}
}

func (s *session) refuse(ctx context.Context, id, msg string) {
	_ = s.send(ctx, protocol.Exit{Type: protocol.TypeExit, ID: id, Code: protocol.ExitRefused, Error: msg})
}

func (s *session) exec(ctx context.Context, req protocol.Exec) {
	cwd, err := s.c.opts.Allow.Check(req.Cwd)
	if err != nil {
		slog.Warn("Refusing command", "id", req.ID, "cwd", req.Cwd, "error", err)
		s.refuse(ctx, req.ID, fmt.Sprintf("device %s refused: working directory %s", s.name, err))
		return
	}
	s.procsMu.Lock()
	_, dup := s.procs[req.ID]
	s.procsMu.Unlock()
	if dup {
		s.refuse(ctx, req.ID, fmt.Sprintf("device %s refused: request id %q is already running", s.name, req.ID))
		return
	}
	var pending []byte
	out := func(b []byte) {
		pending = append(pending, b...)
		cut := completeUTF8(pending)
		if cut > 0 {
			_ = s.send(ctx, protocol.Output{Type: protocol.TypeOutput, ID: req.ID, Data: string(pending[:cut])})
		}
		pending = append(pending[:0], pending[cut:]...)
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	p, err := executor.Start(s.c.opts.Shell, executor.Request{
		Cmd: req.Cmd, Cwd: cwd, Login: req.Login, Timeout: timeout, Stdin: req.Stdin, Env: s.c.opts.Env,
	}, out)
	if err != nil {
		s.refuse(ctx, req.ID, fmt.Sprintf("device %s could not start the command: %s", s.name, err))
		return
	}
	slog.Debug("Started command", "id", req.ID, "pid", p.Pid(), "cwd", cwd, "login", req.Login)
	s.procsMu.Lock()
	s.procs[req.ID] = p
	s.procsMu.Unlock()
	s.wg.Go(func() {
		res, _ := p.Wait(context.WithoutCancel(ctx))
		s.procsMu.Lock()
		delete(s.procs, req.ID)
		s.procsMu.Unlock()
		if len(pending) > 0 {
			_ = s.send(ctx, protocol.Output{Type: protocol.TypeOutput, ID: req.ID, Data: string(pending)})
		}
		_ = s.send(ctx, protocol.Exit{Type: protocol.TypeExit, ID: req.ID, Code: res.Code, Killed: res.Killed})
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
