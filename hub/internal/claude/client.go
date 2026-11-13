// Package claude serves the Claude subscription accounts of Hermes
// backends that run the Claude dashboard plugin: their status
// and usage, the sign-in that the plugin runs with `claude auth login`,
// and a notice when a chat's account lost its login.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/device"
)

// PluginBase is where the Hermes dashboard mounts the Claude
// plugin's routes.
const PluginBase = "/api/plugins/claude-accounts"

const maxBody = 1 << 20

// Plugin error codes.
const (
	CodeUnknownAccount = "unknown_account"
	CodeNotFound       = "not_found"
	CodeWrongSignIn    = "wrong_signin"
	CodeBadCode        = "bad_code"
)

// Status is an account's login as the plugin last checked it.
type Status struct {
	LoggedIn         bool      `json:"logged_in"`
	Email            string    `json:"email"`
	OrgName          string    `json:"org_name"`
	SubscriptionType string    `json:"subscription_type"`
	LoginExpiresAt   time.Time `json:"login_expires_at"`
	CheckedAt        time.Time `json:"checked_at"`
	Error            string    `json:"error"`
}

// Window is one usage window. UsedPercent is nil when unknown.
type Window struct {
	Kind        string    `json:"kind"`
	Label       string    `json:"label"`
	UsedPercent *float64  `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
}

// Usage is an account's usage.
type Usage struct {
	Windows           []Window  `json:"windows"`
	Details           []string  `json:"details"`
	UnavailableReason string    `json:"unavailable_reason"`
	FetchedAt         time.Time `json:"fetched_at"`
}

// SignIn is a sign-in the plugin runs for an account.
type SignIn struct {
	ID        string    `json:"id"`
	Account   string    `json:"account"`
	Label     string    `json:"label"`
	EmailHint string    `json:"email_hint"`
	Reason    string    `json:"reason"`
	State     string    `json:"state"`
	URL       string    `json:"url"`
	Message   string    `json:"message"`
	Email     string    `json:"email"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Credits is the plugin's estimate of an account's API credits.
type Credits struct {
	Provider      string    `json:"provider"`
	MonthlyUSD    *float64  `json:"monthly_usd"`
	SpentUSD      *float64  `json:"spent_usd"`
	LeftUSD       *float64  `json:"left_usd"`
	Since         time.Time `json:"since"`
	ResetsAt      time.Time `json:"resets_at"`
	UnpricedCalls int       `json:"unpriced_calls"`
	Error         string    `json:"error"`
}

// Account is one Claude account slot of the plugin.
type Account struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Provider  string   `json:"provider"`
	ConfigDir string   `json:"config_dir"`
	Status    Status   `json:"status"`
	LastEmail string   `json:"last_email"`
	SignIn    *SignIn  `json:"signin"`
	Usage     *Usage   `json:"usage"`
	Credits   *Credits `json:"api_credits"`
}

// Pool is the plugin's GET /pool answer: the account the provider runs a
// chat's next turn on, or Message when none can.
type Pool struct {
	Account        string    `json:"account"`
	Label          string    `json:"label"`
	Provider       string    `json:"provider"`
	Pinned         bool      `json:"pinned"`
	WeeklyResetsAt time.Time `json:"weekly_resets_at"`
	Message        string    `json:"message"`
}

// Accounts is the plugin's GET /accounts answer.
type Accounts struct {
	CLIVersion string    `json:"cli_version"`
	Accounts   []Account `json:"accounts"`
}

// Error is an error answer of the plugin, or of the dashboard in front of
// it. It never holds what the hub sent.
type Error struct {
	Status int
	Code   string `json:"error"`
}

func (e *Error) Error() string { return fmt.Sprintf("claude-accounts plugin: %d %s", e.Status, e.Code) }

// ErrUnavailable is a plugin that could not be reached or did not answer
// in JSON.
var ErrUnavailable = errors.New("claude-accounts plugin unavailable")

// slowRequester sends a request that only its context bounds.
// *gateway.Dialer is one.
type slowRequester interface {
	RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error)
}

// Client calls the Claude plugin of one backend.
type Client struct {
	r device.Requester
}

// NewClient returns a client that sends its requests through r.
func NewClient(r device.Requester) *Client { return &Client{r: r} }

// call sends one request to the plugin. slow requests (a sign-in waits
// up to a minute) go without the dashboard client's own timeout, bounded
// by ctx alone.
func (c *Client) call(ctx context.Context, slow bool, method, path string, body, out any) error {
	return c.do(ctx, slow, method, PluginBase+path, maxBody, body, out)
}

// do sends one request to the dashboard and reads at most limit bytes of
// the answer.
func (c *Client) do(ctx context.Context, slow bool, method, path string, limit int64, body, out any) error {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return err
		}
	}
	send := c.r.Request
	if s, ok := c.r.(slowRequester); ok && slow {
		send = s.RequestSlow
	}
	resp, err := send(ctx, method, path, b)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode}
		var raw struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(data, &raw) == nil {
			e.Code, _ = raw.Error.(string)
		}
		if e.Code == "" {
			e.Code = http.StatusText(resp.StatusCode)
		}
		return e
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: bad answer to %s %s", ErrUnavailable, method, path)
	}
	return nil
}

func accountPath(id string) string { return "/accounts/" + url.PathEscape(id) }

// Accounts lists the accounts, with their usage when usage is true.
func (c *Client) Accounts(ctx context.Context, usage bool) (Accounts, error) {
	var out Accounts
	q := "?usage=0"
	if usage {
		q = "?usage=1"
	}
	return out, c.call(ctx, false, http.MethodGet, "/accounts"+q, nil, &out)
}

// Pool asks which account the provider runs the session's next turn of
// model on.
func (c *Client) Pool(ctx context.Context, sessionID, model string) (Pool, error) {
	var out Pool
	q := url.Values{"session": {sessionID}, "model": {model}}
	return out, c.call(ctx, false, http.MethodGet, "/pool?"+q.Encode(), nil, &out)
}

// Usage reads one account's usage.
func (c *Client) Usage(ctx context.Context, id string) (Usage, error) {
	var out Usage
	return out, c.call(ctx, false, http.MethodGet, accountPath(id)+"/usage", nil, &out)
}

// Status checks one account's login.
func (c *Client) Status(ctx context.Context, id string) (Account, error) {
	var out Account
	return out, c.call(ctx, false, http.MethodGet, accountPath(id)+"/status", nil, &out)
}

// StartSignIn starts a sign-in for an account, or returns the one that
// runs.
func (c *Client) StartSignIn(ctx context.Context, id string) (SignIn, error) {
	var out SignIn
	return out, c.call(ctx, true, http.MethodPost, accountPath(id)+"/signin", struct{}{}, &out)
}

// SignIn returns the running or last sign-in of an account; ok is false
// when there is none.
func (c *Client) SignIn(ctx context.Context, id string) (SignIn, bool, error) {
	var out SignIn
	err := c.call(ctx, false, http.MethodGet, accountPath(id)+"/signin", nil, &out)
	var pe *Error
	if errors.As(err, &pe) && pe.Status == http.StatusNotFound && pe.Code == CodeNotFound {
		return SignIn{}, false, nil
	}
	return out, err == nil, err
}

// SendCode passes the code from the Claude sign-in page to the sign-in
// signInID. The code goes only into the request body.
func (c *Client) SendCode(ctx context.Context, id, signInID, code string) (SignIn, error) {
	var out SignIn
	body := map[string]string{"signin_id": signInID, "code": code}
	return out, c.call(ctx, true, http.MethodPost, accountPath(id)+"/signin/code", body, &out)
}

// CancelSignIn stops an account's sign-in.
func (c *Client) CancelSignIn(ctx context.Context, id string) (SignIn, error) {
	var out SignIn
	return out, c.call(ctx, false, http.MethodDelete, accountPath(id)+"/signin", nil, &out)
}
