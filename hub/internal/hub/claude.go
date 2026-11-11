package hub

import (
	"context"
	"errors"
	"time"
)

// ClaudeAPIProvider is Hermes's provider of the Anthropic API with an API
// key. The hub shows it as ClaudeAPIName.
const (
	ClaudeAPIProvider = "anthropic"
	ClaudeAPIName     = "Claude API"
)

// ClaudeWindow is one usage window of a Claude subscription account, such
// as the 5-hour session or the week. UsedPercent is nil when unknown.
// ResetsText is ResetsAt in the backend's time zone, such as "3:40 PM CST".
type ClaudeWindow struct {
	Kind        string    `json:"kind"`
	Label       string    `json:"label"`
	UsedPercent *float64  `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
	ResetsText  string    `json:"resets_text,omitempty"`
}

// Claude usage window kinds the hub knows. Other kinds are extra rows.
const (
	ClaudeWindowSession = "session"
	ClaudeWindowWeekly  = "weekly_all"
)

// ClaudeUsage is a Claude account's usage. Unavailable says why there is
// none, when the plugin could not read it.
type ClaudeUsage struct {
	Windows     []ClaudeWindow `json:"windows"`
	Details     []string       `json:"details,omitempty"`
	Unavailable string         `json:"unavailable,omitempty"`
	FetchedAt   time.Time      `json:"fetched_at,omitzero"`
}

// ClaudeAccount is one Claude subscription account of a backend. Email is
// the signed-in email, else the email it was last signed in to.
type ClaudeAccount struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	SignedIn bool   `json:"signed_in"`
	Email    string `json:"email,omitempty"`
	// LastEmail is the email the account was last signed in to.
	LastEmail   string       `json:"last_email,omitempty"`
	Plan        string       `json:"plan,omitempty"`
	StatusError string       `json:"status_error,omitempty"`
	Usage       *ClaudeUsage `json:"usage,omitempty"`
	// SignIn is the running or last sign-in, for admins only.
	SignIn *ClaudeSignIn `json:"signin,omitempty"`
}

// ClaudeAccounts are a backend's Claude accounts. Admin says whether the
// user may sign the accounts in.
type ClaudeAccounts struct {
	BackendID string `json:"backend_id"`
	Admin     bool   `json:"admin"`
	// TimeZone is the IANA zone of the windows' ResetsText.
	TimeZone string          `json:"time_zone"`
	Accounts []ClaudeAccount `json:"accounts"`
}

// Claude sign-in states.
const (
	SignInStarting  = "starting"
	SignInWaiting   = "waiting_code"
	SignInVerifying = "verifying"
	SignInSucceeded = "succeeded"
	SignInFailed    = "failed"
	SignInCancelled = "cancelled"
	SignInTimedOut  = "timed_out"
)

// Claude sign-in reasons.
const (
	SignInFirst   = "first"
	SignInExpired = "expired"
)

// ClaudeSignIn is a `claude auth login` that the plugin runs for one
// account. URL is the page where the person signs in; only admins see it.
type ClaudeSignIn struct {
	ID        string    `json:"id"`
	Account   string    `json:"account"`
	Label     string    `json:"label"`
	EmailHint string    `json:"email_hint,omitempty"`
	Reason    string    `json:"reason"`
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"`
	Message   string    `json:"message,omitempty"`
	Email     string    `json:"email,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// KindClaudeSignIn is a hub event in a chat whose Claude account needs a
// sign-in. Data is a ClaudeSignInNotice.
const KindClaudeSignIn = "claude.signin"

// ClaudeSignInNotice says which Claude account lost its login and why.
type ClaudeSignInNotice struct {
	Account   string `json:"account"`
	Label     string `json:"label"`
	Provider  string `json:"provider"`
	EmailHint string `json:"email_hint,omitempty"`
	Reason    string `json:"reason"`
}

// Claude serves the Claude accounts of backends with the claude-accounts
// Hermes plugin. Every method fails with ErrForbidden when the user may
// not use the backend; the sign-in methods also when the user is not one
// of the backend's Claude admins. ErrNoClaude is a backend without the
// plugin.
type Claude interface {
	ClaudeAccounts(ctx context.Context, user, backendID string, usage bool) (ClaudeAccounts, error)
	// ClaudeUsage reads one account's usage. fresh asks for a newer read
	// than the hub's cache, as after a reply.
	ClaudeUsage(ctx context.Context, user, backendID, accountID string, fresh bool) (ClaudeUsage, error)
	StartSignIn(ctx context.Context, user, backendID, accountID string) (ClaudeSignIn, error)
	SignInState(ctx context.Context, user, backendID, accountID string) (ClaudeSignIn, error)
	// SendSignInCode passes the code from the Claude sign-in page to the
	// running sign-in. The code is never logged or kept.
	SendSignInCode(ctx context.Context, user, backendID, accountID, signInID, code string) (ClaudeSignIn, error)
	CancelSignIn(ctx context.Context, user, backendID, accountID string) (ClaudeSignIn, error)
}

// Claude errors.
var (
	ErrNoClaude       = errors.New("this Hermes has no Claude accounts plugin")
	ErrUnknownAccount = errors.New("unknown Claude account")
	ErrNoSignIn       = errors.New("no sign-in is running for this account")
	ErrWrongSignIn    = errors.New("that sign-in is no longer the current one; start it again")
	ErrBadCode        = errors.New("that code was not accepted; copy the whole code from the Claude page")
)
