// Package bidi is a minimal WebDriver BiDi client. talaria uses it
// only in tests, to load the Hermes extension as a temporary add-on into a
// throwaway Firefox profile.
package bidi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// Client is one BiDi WebSocket session.
type Client struct {
	conn    *websocket.Conn
	next    atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan reply
	done    chan struct{}
	err     error
}

type reply struct {
	Type    string          `json:"type"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

// Dial connects to ws://host:port/session. It sends no Origin header,
// which Firefox requires unless --remote-allow-origins is set.
func Dial(ctx context.Context, host string, port int) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, "ws://"+net.JoinHostPort(host, strconv.Itoa(port))+"/session", nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	c := &Client{conn: conn, pending: map[int64]chan reply{}, done: make(chan struct{})}
	go c.readLoop(context.WithoutCancel(ctx))
	return c, nil
}

// readLoop outlives the dial context; Close ends it.
func (c *Client) readLoop(ctx context.Context) {
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			c.err = err
			return
		}
		var r reply
		if json.Unmarshal(data, &r) != nil || r.ID == nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[*r.ID]
		delete(c.pending, *r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}

// Call sends one command and returns its result.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.next.Add(1)
	data, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Type == "error" {
			return nil, fmt.Errorf("%s: %s: %s", method, r.Error, r.Message)
		}
		return r.Result, nil
	case <-c.done:
		return nil, errors.Join(errors.New("bidi connection closed"), c.err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// NewSession starts a BiDi session.
func (c *Client) NewSession(ctx context.Context) error {
	_, err := c.Call(ctx, "session.new", map[string]any{"capabilities": map[string]any{}})
	return err
}

// InstallTemporary loads the unpacked extension in dir as a temporary
// add-on and returns its id.
func (c *Client) InstallTemporary(ctx context.Context, dir string) (string, error) {
	res, err := c.Call(ctx, "webExtension.install", map[string]any{
		"extensionData": map[string]any{"type": "path", "path": dir},
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Extension string `json:"extension"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.Extension, nil
}

// Close ends the session and the connection.
func (c *Client) Close(ctx context.Context) error {
	_, _ = c.Call(ctx, "session.end", map[string]any{})
	return c.conn.Close(websocket.StatusNormalClosure, "")
}
