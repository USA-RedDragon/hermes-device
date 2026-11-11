// Package hubapi is the device side of the hermes-hub device API described
// in docs/protocol.md: pairing, whoami, session bindings and the three
// WebSocket legs.
package hubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Hub paths.
const (
	PathPair        = "/api/device/pair"
	PathPairToken   = "/api/device/pair/token" //nolint:gosec // a URL path, not a credential
	PathWhoami      = "/api/device/whoami"
	PathBindingFmt  = "/api/device/sessions/%s/binding"
	PathExecLeg     = "/api/device/exec"
	PathACPLeg      = "/api/device/acp"
	PathGatewayLeg  = "/api/device/gateway"
	PathUsage       = "/api/device/usage"
	maxResponseBody = 1 << 20
)

// Pairing poll errors, the error codes of RFC 8628 section 3.5.
const (
	PairPending  = "authorization_pending"
	PairSlowDown = "slow_down"
	PairExpired  = "expired_token"
	PairDenied   = "access_denied"
)

// ErrUnauthorized is returned when the hub refuses the device token. The
// token is revoked or wrong; only login fixes it.
var ErrUnauthorized = errors.New("hub refused the device token; run talaria login")

// Error is an error answer from the hub.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("hub: %s (%d %s)", e.Message, e.Status, e.Code)
	}
	return fmt.Sprintf("hub: %d %s", e.Status, e.Code)
}

// PairRequest starts pairing.
type PairRequest struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	ClientVersion string `json:"client_version"`
}

// PairStart is the hub's answer to PairRequest.
type PairStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// PairResult is the hub's answer once the user confirmed the device.
type PairResult struct {
	Token      string `json:"token"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Backend    string `json:"backend"`
}

// Whoami describes the device the token belongs to and its backend.
type Whoami struct {
	DeviceID      string `json:"device_id"`
	DeviceName    string `json:"device_name"`
	User          string `json:"user"`
	Backend       string `json:"backend"`
	HermesVersion string `json:"hermes_version"`
	PluginVersion string `json:"plugin_version"`
	Placeholder   string `json:"placeholder"`
}

// Binding is a session's device binding.
type Binding struct {
	SessionID  string  `json:"session_id"`
	DeviceID   string  `json:"device_id"`
	Dir        string  `json:"dir"`
	Generation string  `json:"generation"`
	BoundAt    float64 `json:"bound_at"`
}

// Client calls the hub.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// UserAgent is sent on every request.
	UserAgent string
}

// New returns a client for the hub at baseURL.
func New(baseURL, token, userAgent string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Token: token, UserAgent: userAgent,
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, auth bool) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized && auth {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Code == "" {
			e.Code = http.StatusText(resp.StatusCode)
		}
		return e
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("hub: bad response to %s %s: %w", method, path, err)
	}
	return nil
}

// StartPairing asks the hub for a pairing code.
func (c *Client) StartPairing(ctx context.Context, req PairRequest) (*PairStart, error) {
	var out PairStart
	if err := c.do(ctx, http.MethodPost, PathPair, req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollPairing asks once whether the user confirmed the device. It returns
// an *Error whose Code is one of the Pair* constants while pairing is not
// done.
func (c *Client) PollPairing(ctx context.Context, deviceCode string) (*PairResult, error) {
	var out PairResult
	if err := c.do(ctx, http.MethodPost, PathPairToken, map[string]string{"device_code": deviceCode}, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendUsage posts a Claude Code usage report (docs/protocol.md section 13).
func (c *Client) SendUsage(ctx context.Context, report any) error {
	return c.do(ctx, http.MethodPost, PathUsage, report, nil, true)
}

// Whoami returns the device and backend the token belongs to.
func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	var out Whoami
	if err := c.do(ctx, http.MethodGet, PathWhoami, nil, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// Bind binds sessionID to this device's directory dir. Binding again to
// the same directory returns the existing binding.
func (c *Client) Bind(ctx context.Context, sessionID, dir string) (*Binding, error) {
	var out Binding
	path := fmt.Sprintf(PathBindingFmt, url.PathEscape(sessionID))
	if err := c.do(ctx, http.MethodPut, path, map[string]string{"dir": dir}, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// Unbind removes sessionID's binding.
func (c *Client) Unbind(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf(PathBindingFmt, url.PathEscape(sessionID)), nil, nil, true)
}

// DialLeg opens one of the WebSocket legs (PathExecLeg, PathACPLeg or
// PathGatewayLeg). A 401 answer to the upgrade returns ErrUnauthorized.
func (c *Client) DialLeg(ctx context.Context, path string) (*websocket.Conn, error) {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.Token)
	if c.UserAgent != "" {
		h.Set("User-Agent", c.UserAgent)
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h, HTTPClient: &http.Client{}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	conn.SetReadLimit(64 << 20)
	return conn, nil
}
