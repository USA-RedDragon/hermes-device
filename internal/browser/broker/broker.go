// Package broker is the daemon side of browser control. It listens on a
// private Unix socket for the native messaging host that Firefox starts for
// the Hermes extension, and sends the extension requests (docs/browser.md).
package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/browser/nativemsg"
)

// SocketName is the socket's file name in the state directory.
const SocketName = "browser.sock"

// SocketEnv overrides the socket path for the native host.
const SocketEnv = "HERMES_DEVICE_BROWSER_SOCKET"

// maxSocketPath is the longest Unix socket path Linux and macOS accept.
const maxSocketPath = 103

// Message types between the broker and the extension.
const (
	TypeHostHello = "host_hello"
	TypeHello     = "hello"
	TypeRequest   = "request"
	TypeResponse  = "response"
	TypeEvent     = "event"
	TypeConfig    = "config"
	// TypeReplaced tells an extension that a newer connection took its
	// place, so it must not reconnect on its own.
	TypeReplaced = "replaced"
)

// CodeRefused is the code of an answer the extension refused on purpose.
const CodeRefused = "refused"

// Errors from Call.
var (
	ErrNotConnected = errors.New("the Hermes Firefox extension is not connected")
	ErrClosed       = errors.New("the Hermes Firefox extension disconnected")
)

// Message is every frame on the socket. Unused fields are empty.
type Message struct {
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	Op         string          `json:"op,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	OK         bool            `json:"ok,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Code       string          `json:"code,omitempty"`
	AllowHosts []string        `json:"allow_hosts,omitempty"`
	// ConfirmHosts are sites, besides Salesforce, where every click asks
	// the user (docs/browser.md section 7.1).
	ConfirmHosts []string `json:"confirm_hosts,omitempty"`
	// AutologinHosts are exact hosts where browser_login may submit the
	// login form Firefox filled (docs/browser.md section 7.4).
	AutologinHosts []string `json:"autologin_hosts,omitempty"`
	ExtensionID    string   `json:"extension_id,omitempty"`
	HostPID        int      `json:"host_pid,omitempty"`
	Version        string   `json:"version,omitempty"`
	Browser        string   `json:"browser,omitempty"`
	// HubURL is the daemon's hub.url. The extension keeps agent tabs off
	// the hub.
	HubURL string `json:"hub_url,omitempty"`
	// State is only in the legacyAuth message.
	State string `json:"state,omitempty"`
}

// legacyAuth lets an extension from releases 0.7.0 to 0.14.0, which ran
// requests only after the daemon reported a hub sign-in, keep working
// until Firefox updates it. Later extensions ignore it.
var legacyAuth = Message{Type: "auth_status", State: "signed_in"} //nolint:gochecknoglobals // fixed message

// Hello describes the connected extension.
type Hello struct {
	ExtensionID string
	Version     string
	Browser     string
	HostPID     int
}

// Options configure a Broker.
type Options struct {
	// Path of the socket. Its directory is created with mode 0700.
	Path string
	// ExtensionID is the only extension id the broker accepts.
	ExtensionID string
	// AllowHosts are loopback or private network host names the extension
	// may open in agent tabs. All others are blocked there.
	AllowHosts []string
	// ConfirmHosts are host names (with their subdomains) where every
	// click and every Enter or Space key press needs the user's OK, and
	// scripts may only read, as on Salesforce.
	ConfirmHosts []string
	// AutologinHosts are exact host names where browser_login may submit
	// the login form that Firefox filled from its saved logins.
	AutologinHosts []string
	// HelloTimeout bounds the handshake. Defaults to 10 seconds.
	HelloTimeout time.Duration
	// HubURL is the daemon's hub.url, which agent tabs may not open.
	HubURL string
}

// Error is an operation the extension answered with ok false.
type Error struct {
	Op      string
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Op + ": " + e.Message }

// Refused reports whether the extension refused the operation, as opposed
// to trying it and failing.
func (e *Error) Refused() bool { return e.Code == CodeRefused }

// Broker serves one extension connection at a time; a newer one replaces
// the older.
type Broker struct {
	opts Options
	ln   *net.UnixListener

	mu      sync.Mutex
	cur     *conn
	changed chan struct{}
	closed  bool
}

type conn struct {
	c       net.Conn
	hello   Hello
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan Message
	done    chan struct{}
}

// Listen creates the socket. A stale socket file at Path is removed.
func Listen(opts Options) (*Broker, error) {
	if opts.Path == "" || opts.ExtensionID == "" {
		return nil, errors.New("broker: Path and ExtensionID are required")
	}
	if len(opts.Path) > maxSocketPath {
		return nil, fmt.Errorf("broker: socket path %s is longer than %d bytes", opts.Path, maxSocketPath)
	}
	if opts.HelloTimeout <= 0 {
		opts.HelloTimeout = 10 * time.Second
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(opts.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: opts.Path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(opts.Path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Broker{opts: opts, ln: ln, changed: make(chan struct{})}, nil
}

// Serve accepts host connections until ctx ends.
func (b *Broker) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = b.ln.Close()
	}()
	for {
		c, err := b.ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				b.drop(nil)
				return nil //nolint:nilerr // shutdown is not an error
			}
			return err
		}
		if err := checkPeer(c); err != nil {
			slog.Warn("Refusing browser host connection", "error", err)
			_ = c.Close()
			continue
		}
		go b.handshake(c)
	}
}

func (b *Broker) handshake(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(b.opts.HelloTimeout))
	var hh, eh Message
	if err := readMsg(c, &hh); err != nil || hh.Type != TypeHostHello {
		slog.Warn("Browser host sent no host_hello", "error", err)
		_ = c.Close()
		return
	}
	if hh.ExtensionID != b.opts.ExtensionID {
		slog.Warn("Refusing browser host for another extension", "extension_id", hh.ExtensionID)
		_ = c.Close()
		return
	}
	if err := readMsg(c, &eh); err != nil || eh.Type != TypeHello {
		slog.Warn("Browser extension sent no hello", "error", err)
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	cn := &conn{
		c:       c,
		hello:   Hello{ExtensionID: hh.ExtensionID, Version: eh.Version, Browser: eh.Browser, HostPID: hh.HostPID},
		pending: map[string]chan Message{},
		done:    make(chan struct{}),
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = c.Close()
		return
	}
	if err := cn.write(b.configLocked()); err != nil {
		b.mu.Unlock()
		slog.Warn("Could not configure the browser extension", "error", err)
		_ = c.Close()
		return
	}
	_ = cn.write(legacyAuth)
	old := b.cur
	b.cur = cn
	b.notifyLocked()
	b.mu.Unlock()
	if old != nil {
		_ = old.write(Message{Type: TypeReplaced})
		old.close()
	}
	slog.Info("Browser extension connected", "version", eh.Version, "browser", eh.Browser, "host_pid", hh.HostPID)
	b.readLoop(cn)
	b.drop(cn)
}

func (b *Broker) drop(cn *conn) {
	b.mu.Lock()
	if cn == nil {
		b.closed = true
		cn = b.cur
	}
	if b.cur == cn && cn != nil {
		b.cur = nil
		b.notifyLocked()
	}
	b.mu.Unlock()
	if cn != nil {
		cn.close()
	}
}

func (b *Broker) configLocked() Message {
	return Message{Type: TypeConfig, AllowHosts: b.opts.AllowHosts, ConfirmHosts: b.opts.ConfirmHosts,
		AutologinHosts: b.opts.AutologinHosts, HubURL: b.opts.HubURL}
}

// notifyLocked wakes everyone waiting on Changed. Call with mu held.
func (b *Broker) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// Changed returns a channel that is closed the next time an extension
// connects or goes away.
func (b *Broker) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

// Connected returns the current extension, if any.
func (b *Broker) Connected() (Hello, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return Hello{}, false
	}
	return b.cur.hello, true
}

// WaitConnected blocks until an extension is connected or ctx ends.
func (b *Broker) WaitConnected(ctx context.Context) (Hello, error) {
	for {
		b.mu.Lock()
		cur, ch := b.cur, b.changed
		b.mu.Unlock()
		if cur != nil {
			return cur.hello, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return Hello{}, ctx.Err()
		}
	}
}

// Call sends op with args to the extension and waits for its answer.
func (b *Broker) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	b.mu.Lock()
	cn := b.cur
	b.mu.Unlock()
	if cn == nil {
		return nil, ErrNotConnected
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	id := newID()
	ch := make(chan Message, 1)
	cn.mu.Lock()
	cn.pending[id] = ch
	cn.mu.Unlock()
	defer func() {
		cn.mu.Lock()
		delete(cn.pending, id)
		cn.mu.Unlock()
	}()
	if err := cn.write(Message{Type: TypeRequest, ID: id, Op: op, Args: raw}); err != nil {
		cn.close()
		return nil, fmt.Errorf("%w: %w", ErrClosed, err)
	}
	select {
	case m := <-ch:
		if !m.OK {
			return nil, &Error{Op: op, Code: m.Code, Message: m.Error}
		}
		return m.Result, nil
	case <-cn.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (cn *conn) write(m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	cn.writeMu.Lock()
	defer cn.writeMu.Unlock()
	_ = cn.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return nativemsg.Write(cn.c, data, nativemsg.MaxToBrowser)
}

func (b *Broker) readLoop(cn *conn) {
	for {
		var m Message
		if err := readMsg(cn.c, &m); err != nil {
			slog.Info("Browser extension disconnected", "error", err)
			return
		}
		switch m.Type {
		case TypeResponse:
			cn.mu.Lock()
			ch := cn.pending[m.ID]
			cn.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
		case TypeEvent:
			slog.Debug("Browser event", "op", m.Op)
		default:
			slog.Debug("Ignoring browser message", "type", m.Type)
		}
	}
}

func (cn *conn) close() {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	select {
	case <-cn.done:
	default:
		close(cn.done)
		_ = cn.c.Close()
	}
}

func readMsg(c net.Conn, m *Message) error {
	data, err := nativemsg.Read(c, nativemsg.MaxFromBrowser)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, m)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
