package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Errors from the client.
var (
	ErrClosed       = errors.New("gateway connection closed")
	ErrNoAuthority  = errors.New("gateway has no session authority")
	ErrProtocol     = errors.New("gateway protocol mismatch")
	ErrUnauthorized = errors.New("gateway login failed")
	// ErrNotReady is a gateway that is starting or draining for a restart:
	// it answers HTTP 503 gateway_not_ready, or closes a socket with 1013.
	ErrNotReady = errors.New("gateway is restarting")
	// ErrUnreachable is a gateway the hub cannot connect to.
	ErrUnreachable = errors.New("gateway unreachable")
)

// statusTryAgainLater is the WebSocket close code Hermes uses while it is
// not ready.
const statusTryAgainLater = websocket.StatusCode(1013)

// Retryable reports whether err means the backend is restarting or cannot
// be reached right now, so the same request may work later: a refused or
// lost connection, a gateway that is not ready, or a runtime_draining
// refusal.
func Retryable(err error) bool {
	var rerr *RPCError
	if errors.As(err, &rerr) {
		return rerr.Message == ReasonRuntimeDraining
	}
	return errors.Is(err, ErrNotReady) || errors.Is(err, ErrUnreachable) || errors.Is(err, ErrClosed) ||
		errors.Is(err, ErrNoAuthority)
}

// RPCError is a JSON-RPC error answer from the gateway.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("gateway error %d: %s", e.Code, e.Message)
}

// Credentials sign in to a Hermes Webapp listener with a dashboard password
// provider.
type Credentials struct {
	BaseURL  string
	Provider string
	Username string
	Password string
}

// Dialer opens authenticated gateway connections to one backend. It keeps
// the dashboard session cookies between dials and signs in again when they
// stop working.
type Dialer struct {
	creds Credentials
	http  *http.Client
	// slow shares the cookies of http but has no time limit of its own:
	// the caller's context bounds it.
	slow *http.Client
}

// attemptTimeout bounds one dashboard call.
const attemptTimeout = 15 * time.Second

// NewDialer returns a Dialer for creds.
func NewDialer(creds Credentials) *Dialer {
	return newDialer(creds, attemptTimeout)
}

func newDialer(creds Credentials, attempt time.Duration) *Dialer {
	jar, _ := cookiejar.New(nil)
	return &Dialer{creds: creds, http: &http.Client{Jar: jar, Timeout: attempt}, slow: &http.Client{Jar: jar}}
}

// restartRetryWindow bounds how long a call is tried again when a gateway
// held it past the attempt limit. A Hermes gateway that is stopping for a
// restart holds what it cannot serve until the next gateway has taken over
// (gateway.restart_listener_hold_seconds), then forwards it, so the next
// attempt is answered.
const restartRetryWindow = 90 * time.Second

// held reports whether a call ran out of its attempt time after it was sent:
// what a gateway that holds it across a restart causes. A refused or lost
// connection is not one: callers still see ErrUnreachable at once.
func held(err error) bool {
	var ne net.Error
	return errors.Is(err, ErrUnreachable) && errors.As(err, &ne) && ne.Timeout()
}

// repeatable reports whether a request with this method may be sent again
// when its first attempt may already have run (RFC 9110 idempotent methods).
func repeatable(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// again runs f, and while it fails with held and safe is true, runs it
// again within ctx and restartRetryWindow.
func again[T any](ctx context.Context, safe bool, f func() (T, error)) (T, error) {
	deadline := time.Now().Add(restartRetryWindow)
	delay := 100 * time.Millisecond
	for {
		v, err := f()
		if err == nil || !safe || !held(err) || ctx.Err() != nil || !time.Now().Before(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
}

const maxFrame = 64 << 20

// Dial signs in if needed, mints a single-use gateway socket ticket and dials
// /api/ws the way the Hermes Webapp page does: native_dial=browser and an
// Origin naming the listener. With an Origin, Hermes sends the dial to the
// Webapp's ticket gate, never to its loopback-only native gate. onEvent gets every event notification in the order the gateway
// sent them, on the connection's reader goroutine.
func (d *Dialer) Dial(ctx context.Context, onEvent func(Event)) (*Client, error) {
	conn, err := d.DialRaw(ctx)
	if err != nil {
		return nil, err
	}
	return newClient(ctx, conn, onEvent)
}

// DialRaw dials the gateway socket as Dial does and returns it before
// anything is read from it, so the gateway.ready event is its first
// message. The caller owns the connection.
func (d *Dialer) DialRaw(ctx context.Context) (*websocket.Conn, error) {
	ticket, err := d.ticket(ctx)
	if errors.Is(err, ErrUnauthorized) {
		if err = d.login(ctx); err != nil {
			return nil, err
		}
		ticket, err = d.ticket(ctx)
	}
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(d.creds.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway url: %w", err)
	}
	origin := base.Scheme + "://" + base.Host
	conn, resp, err := websocket.Dial(ctx, WebSocketURL(d.creds.BaseURL)+"/api/ws?native_dial=browser", &websocket.DialOptions{
		Subprotocols: []string{Subprotocol, TicketPrefix + ticket},
		HTTPHeader:   http.Header{"Origin": []string{origin}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	switch {
	case err != nil && resp == nil:
		return nil, fmt.Errorf("dial gateway: %w: %w", ErrUnreachable, err)
	case err != nil && resp.StatusCode == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("dial gateway: %w: %w", ErrNotReady, err)
	case err != nil:
		return nil, fmt.Errorf("dial gateway: %w", err)
	}
	if conn.Subprotocol() != Subprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "protocol")
		return nil, ErrProtocol
	}
	conn.SetReadLimit(maxFrame)
	return conn, nil
}

// WebSocketURL turns an http or https base URL into ws or wss.
func WebSocketURL(base string) string {
	if rest, ok := strings.CutPrefix(base, "https:"); ok {
		return "wss:" + rest
	}
	if rest, ok := strings.CutPrefix(base, "http:"); ok {
		return "ws:" + rest
	}
	return base
}

// Request sends one HTTP request to the listener with the dashboard
// session, signing in again once when it is refused with 401. body may be
// nil. The caller closes the response body.
//
// A request that a restarting gateway held past the attempt limit is sent
// again when its method is idempotent.
func (d *Dialer) Request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return again(ctx, repeatable(method), func() (*http.Response, error) {
		return d.request(ctx, d.http, method, path, body)
	})
}

// RequestSlow is Request with only ctx to bound it, for a request that
// may take longer than a dashboard call, such as an upload.
func (d *Dialer) RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return again(ctx, repeatable(method), func() (*http.Response, error) {
		return d.request(ctx, d.slow, method, path, body)
	})
}

func (d *Dialer) request(ctx context.Context, c *http.Client, method, path string, body []byte) (*http.Response, error) {
	resp, err := d.send(ctx, c, method, path, body)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err := d.login(ctx); err != nil {
		return nil, err
	}
	return d.send(ctx, c, method, path, body)
}

func (d *Dialer) send(ctx context.Context, c *http.Client, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.creds.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %w", method, path, ErrUnreachable, err)
	}
	return resp, nil
}

func (d *Dialer) post(ctx context.Context, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.creds.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.http.Do(req)
}

// login signs in. Signing in again is harmless, so a held attempt is
// repeated.
func (d *Dialer) login(ctx context.Context) error {
	_, err := again(ctx, true, func() (struct{}, error) { return struct{}{}, d.loginOnce(ctx) })
	return err
}

func (d *Dialer) loginOnce(ctx context.Context) error {
	resp, err := d.post(ctx, "/auth/password-login", map[string]string{
		"provider": d.creds.Provider, "username": d.creds.Username, "password": d.creds.Password,
	})
	if err != nil {
		return fmt.Errorf("gateway login: %w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("gateway login: %w", ErrNotReady)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	}
	return nil
}

// ticket mints a socket ticket. An unused ticket expires on its own, so a
// held attempt is repeated.
func (d *Dialer) ticket(ctx context.Context) (string, error) {
	return again(ctx, true, func() (string, error) { return d.ticketOnce(ctx) })
}

func (d *Dialer) ticketOnce(ctx context.Context) (string, error) {
	resp, err := d.post(ctx, "/api/webapp/socket-ticket?socket=gateway", nil)
	if err != nil {
		return "", fmt.Errorf("gateway ticket: %w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("gateway ticket: %w", ErrNotReady)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("gateway ticket: status %d", resp.StatusCode)
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.Ticket == "" {
		return "", fmt.Errorf("gateway ticket: bad answer: %w", err)
	}
	return out.Ticket, nil
}

type frame struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type answer struct {
	result json.RawMessage
	err    error
}

// Client is one gateway WebSocket connection.
type Client struct {
	conn    *websocket.Conn
	onEvent func(Event)

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan answer
	done    chan struct{}
	err     error
}

const readyTimeout = 10 * time.Second

func newClient(ctx context.Context, conn *websocket.Conn, onEvent func(Event)) (*Client, error) {
	c := &Client{conn: conn, onEvent: onEvent, pending: map[int64]chan answer{}, done: make(chan struct{})}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	var ready struct {
		Type    string `json:"type"`
		Payload struct {
			SessionAuthority bool `json:"session_authority"`
		} `json:"payload"`
	}
	for ready.Type != ReadyEvent {
		_, data, err := conn.Read(readyCtx)
		if err != nil {
			_ = conn.CloseNow()
			return nil, fmt.Errorf("wait for gateway.ready: %w: %w", ErrNotReady, err)
		}
		var f frame
		if json.Unmarshal(data, &f) != nil || f.Method != "event" {
			continue
		}
		_ = json.Unmarshal(f.Params, &ready)
	}
	if !ready.Payload.SessionAuthority {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return nil, ErrNoAuthority
	}
	go c.read() //nolint:gosec // the reader outlives the dial context by design
	return c, nil
}

// read runs for the life of the connection, past any request context.
//
//nolint:contextcheck // see above
func (c *Client) read() {
	var err error
	defer func() { c.fail(err) }()
	for {
		var data []byte
		_, data, err = c.conn.Read(context.Background())
		if err != nil {
			return
		}
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch {
		case f.ID != nil && f.Method == "":
			c.mu.Lock()
			ch := c.pending[*f.ID]
			delete(c.pending, *f.ID)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if f.Error != nil {
				ch <- answer{err: f.Error}
			} else {
				ch <- answer{result: f.Result}
			}
		case f.Method == "event":
			var ev Event
			if json.Unmarshal(f.Params, &ev) == nil && c.onEvent != nil {
				c.onEvent(ev)
			}
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	if websocket.CloseStatus(err) == statusTryAgainLater {
		err = fmt.Errorf("%w: %w", ErrNotReady, err)
	}
	c.err = fmt.Errorf("%w: %w", ErrClosed, err)
	for id, ch := range c.pending {
		ch <- answer{err: c.err}
		delete(c.pending, id)
	}
	close(c.done)
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the connection ended.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection. Sessions stay owned by the gateway.
func (c *Client) Close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	c.fail(ErrClosed)
}

// Call sends one request and decodes its result into out (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan answer, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return c.err
	default:
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	msg, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", id, method, raw})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, msg)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case a := <-ch:
		if a.err != nil {
			return fmt.Errorf("%s: %w", method, a.err)
		}
		if out != nil && len(a.result) > 0 {
			if err := json.Unmarshal(a.result, out); err != nil {
				return fmt.Errorf("%s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// ErrNoFile is a file the listener does not serve: missing, not a regular
// file, refused by its read rules, or too large.
var ErrNoFile = errors.New("the backend has no such file")

// File opens a file of the listener's server the way the Hermes Webapp
// page downloads one: POST /api/files/ticket mints a ticket bound to the
// path, and GET /api/fs/download with that ticket streams the file. Only
// ctx bounds the download. The caller closes the response body.
func (d *Dialer) File(ctx context.Context, path string) (*http.Response, error) {
	body, err := json.Marshal(map[string]string{"route": "download", "path": path})
	if err != nil {
		return nil, err
	}
	resp, err := d.Request(ctx, http.MethodPost, "/api/files/ticket", body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	err = fileStatus(resp, "file ticket")
	if err == nil && (json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil || out.Ticket == "") {
		err = errors.New("file ticket: bad answer")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	q := url.Values{"path": {path}, "ticket": {out.Ticket}}
	resp, err = d.send(ctx, d.slow, http.MethodGet, "/api/fs/download?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if err := fileStatus(resp, "download"); err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func fileStatus(resp *http.Response, what string) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return fmt.Errorf("%s: %w: status %d", what, ErrNoFile, resp.StatusCode)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%s: %w", what, ErrNotReady)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: %w", what, ErrUnauthorized)
	}
	return fmt.Errorf("%s: status %d", what, resp.StatusCode)
}
