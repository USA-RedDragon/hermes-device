package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
)

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
}

// NewDialer returns a Dialer for creds.
func NewDialer(creds Credentials) *Dialer {
	jar, _ := cookiejar.New(nil)
	return &Dialer{creds: creds, http: &http.Client{Jar: jar, Timeout: 15 * time.Second}}
}

const maxFrame = 64 << 20

// Dial signs in if needed, mints a single-use WebSocket ticket and dials
// /api/ws. onEvent gets every event notification in the order the gateway
// sent them, on the connection's reader goroutine.
func (d *Dialer) Dial(ctx context.Context, onEvent func(Event)) (*Client, error) {
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
	wsURL := strings.Replace(strings.Replace(d.creds.BaseURL, "https:", "wss:", 1), "http:", "ws:", 1) + "/api/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{Subprotocol, TicketPrefix + ticket},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dial gateway: %w", err)
	}
	if conn.Subprotocol() != Subprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "protocol")
		return nil, ErrProtocol
	}
	conn.SetReadLimit(maxFrame)
	return newClient(ctx, conn, onEvent)
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

func (d *Dialer) login(ctx context.Context) error {
	resp, err := d.post(ctx, "/auth/password-login", map[string]string{
		"provider": d.creds.Provider, "username": d.creds.Username, "password": d.creds.Password,
	})
	if err != nil {
		return fmt.Errorf("gateway login: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	}
	return nil
}

func (d *Dialer) ticket(ctx context.Context) (string, error) {
	resp, err := d.post(ctx, "/api/auth/ws-ticket", nil)
	if err != nil {
		return "", fmt.Errorf("gateway ticket: %w", err)
	}
	defer resp.Body.Close()
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
			return nil, fmt.Errorf("wait for gateway.ready: %w", err)
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
	go c.read()
	return c, nil
}

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
