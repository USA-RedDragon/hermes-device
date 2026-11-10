// Package client keeps one WebSocket connection to the Hermes device plugin
// open and runs the commands the server sends.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-device/client/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/client/internal/config"
	"github.com/USA-RedDragon/hermes-device/client/internal/executor"
	"github.com/USA-RedDragon/hermes-device/client/internal/protocol"
	"github.com/coder/websocket"
)

// ErrUnauthorized is returned when the server refuses the device name or
// token. Reconnecting would not help.
var ErrUnauthorized = errors.New("server refused the device name or token")

// ErrUnexpectedMessage is returned when the server does not answer Hello
// with Welcome.
var ErrUnexpectedMessage = errors.New("unexpected message from server")

const (
	helloTimeout = 10 * time.Second
	writeTimeout = 30 * time.Second
	// A connection that stayed up this long resets the backoff.
	stableAfter = 30 * time.Second
)

// Client is the device side of the protocol.
type Client struct {
	cfg   *config.Config
	allow *allowlist.List
	dial  func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns a client for cfg. Roots are resolved now, so a missing root
// fails at startup.
func New(cfg *config.Config) (*Client, error) {
	allow, err := allowlist.New(cfg.Roots)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, allow: allow, dial: websocket.Dial, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run connects and serves until ctx ends or the server refuses the device.
// Lost connections are retried with exponential backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.Reconnect.MinBackoff
	for {
		started := time.Now()
		err := c.serveOnce(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown is not an error
		}
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		if time.Since(started) > stableAfter {
			backoff = c.cfg.Reconnect.MinBackoff
		}
		slog.Warn("Connection lost; reconnecting", "error", err, "in", backoff)
		if err := c.sleep(ctx, backoff); err != nil {
			return nil //nolint:nilerr // shutdown is not an error
		}
		backoff = min(backoff*2, c.cfg.Reconnect.MaxBackoff)
	}
}

func (c *Client) tempDir() string {
	if c.cfg.TempDir != "" {
		return c.cfg.TempDir
	}
	return os.TempDir()
}

type session struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	procsMu sync.Mutex
	procs   map[string]*executor.Process
	wg      sync.WaitGroup
}

func (s *session) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
}

func (c *Client) serveOnce(ctx context.Context) error {
	conn, resp, err := c.dial(ctx, c.cfg.Server.URL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(64 << 20)
	defer conn.CloseNow()

	s := &session{conn: conn, procs: map[string]*executor.Process{}}
	connCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.killAll()
		s.wg.Wait()
	}()

	hostname, _ := os.Hostname()
	hello := protocol.Hello{
		Type: protocol.TypeHello, Version: protocol.Version, Name: c.cfg.Device.Name, Token: c.cfg.Device.Token,
		Roots: c.allow.Roots(), TempDir: c.tempDir(), Hostname: hostname, OS: runtime.GOOS,
	}
	if err := s.send(connCtx, hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	hctx, hcancel := context.WithTimeout(connCtx, helloTimeout)
	_, data, err := conn.Read(hctx)
	hcancel()
	if err != nil {
		if websocket.CloseStatus(err) == protocol.CloseUnauthorized {
			return ErrUnauthorized
		}
		return fmt.Errorf("welcome: %w", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Type != protocol.TypeWelcome {
		return fmt.Errorf("%w: %s", ErrUnexpectedMessage, truncate(data))
	}
	slog.Info("Connected", "url", c.cfg.Server.URL, "device", c.cfg.Device.Name, "roots", c.allow.Roots())

	for {
		_, data, err := conn.Read(connCtx)
		if err != nil {
			if websocket.CloseStatus(err) == protocol.CloseUnauthorized {
				return ErrUnauthorized
			}
			return fmt.Errorf("read: %w", err)
		}
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
			c.exec(connCtx, s, req)
		case protocol.TypeKill:
			var req protocol.Kill
			if err := json.Unmarshal(data, &req); err == nil {
				s.kill(req.ID)
			}
		case protocol.TypePing:
			_ = s.send(connCtx, protocol.Envelope{Type: protocol.TypePong})
		default:
			slog.Debug("Ignoring message", "type", env.Type)
		}
	}
}

func truncate(b []byte) string {
	const limit = 200
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
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

func (c *Client) exec(ctx context.Context, s *session, req protocol.Exec) {
	cwd, err := c.allow.Check(req.Cwd)
	if err != nil {
		slog.Warn("Refusing command", "id", req.ID, "cwd", req.Cwd, "error", err)
		_ = s.send(ctx, protocol.Exit{
			Type: protocol.TypeExit, ID: req.ID, Code: protocol.ExitRefused,
			Error: fmt.Sprintf("device %s refused: working directory %s", c.cfg.Device.Name, err),
		})
		return
	}
	var pending []byte
	out := func(b []byte) {
		pending = append(pending, b...)
		// Send only whole UTF-8 sequences so a chunk boundary never splits
		// a character; the JSON string must be valid UTF-8.
		cut := len(pending)
		for cut > 0 && cut > len(pending)-utf8.UTFMax && !utf8.Valid(pending[:cut]) {
			cut--
		}
		if !utf8.Valid(pending[:cut]) {
			cut = len(pending)
		}
		chunk := pending[:cut]
		pending = append([]byte(nil), pending[cut:]...)
		if len(chunk) > 0 {
			_ = s.send(ctx, protocol.Output{Type: protocol.TypeOutput, ID: req.ID, Data: string(chunk)})
		}
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	p, err := executor.Start(c.cfg.Shell, executor.Request{
		Cmd: req.Cmd, Cwd: cwd, Login: req.Login, Timeout: timeout, Stdin: req.Stdin, Env: os.Environ(),
	}, out)
	if err != nil {
		_ = s.send(ctx, protocol.Exit{Type: protocol.TypeExit, ID: req.ID, Code: protocol.ExitRefused, Error: "start: " + err.Error()})
		return
	}
	slog.Debug("Started command", "id", req.ID, "pid", p.Pid(), "cwd", cwd, "login", req.Login)
	s.procsMu.Lock()
	s.procs[req.ID] = p
	s.procsMu.Unlock()
	s.wg.Go(func() {
		res, _ := p.Wait(context.Background())
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
