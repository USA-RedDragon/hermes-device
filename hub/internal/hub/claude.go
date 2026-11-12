package hub

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ClaudeAPIProvider is Hermes's provider of the Anthropic API with an API
// key. The hub shows it as ClaudeAPIName.
const (
	ClaudeAPIProvider = "anthropic"
	ClaudeAPIName     = "Claude API"
)

// ClaudePoolProvider is the claude-accounts plugin's provider that runs
// each chat on one of the backend's Claude subscription accounts. The
// pickers show its models under ClaudePoolGroup.
const (
	ClaudePoolProvider = "claude-pool"
	ClaudePoolGroup    = "Claude"
)

// ClaudeKeysPrefix starts the provider of one Claude account's API key,
// "claude-api-<account>", which bills the account's API credits. The
// pickers show these providers' models under ClaudeKeysGroup.
const (
	ClaudeKeysPrefix = "claude-api-"
	ClaudeKeysGroup  = "Claude API credits"
)

// ClaudeModelName names a Claude model as Anthropic does: "Opus 5.5" for
// claude-opus-5-5 and claude-opus-5-5[1m]. A snapshot date is not part of
// the name. ok is false for an ID that is not a versioned Claude model.
func ClaudeModelName(id string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(id, "[1m]"), "claude-")
	if !ok || rest == "" {
		return "", false
	}
	parts := strings.Split(rest, "-")
	if last := parts[len(parts)-1]; len(parts) > 1 && len(last) == 8 && isDigits(last) {
		parts = parts[:len(parts)-1]
	}
	family := parts[0]
	if family == "" || isDigits(family) {
		return "", false
	}
	var version []string
	for _, p := range parts[1:] {
		if !isDigits(p) {
			return "", false
		}
		version = append(version, p)
	}
	if len(version) == 0 {
		return "", false
	}
	return strings.ToUpper(family[:1]) + family[1:] + " " + strings.Join(version, "."), true
}

func isDigits(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return s != "" && err == nil
}

// ArrangeModels sets each provider's Group and each model's Label as the
// pickers show them, and puts the Claude groups first: the Claude pool's
// models by name under "Claude", then every account's API key models under
// "Claude API credits" as "Opus 5.5 (caam credits)". Other providers keep
// their name, IDs and order.
func ArrangeModels(providers []ModelProvider) {
	for i := range providers {
		if providers[i].ID == ClaudePoolProvider {
			providers[i].Models = newestPerFamily(providers[i].Models)
		}
	}
	labelModels(providers)
	rank := func(p ModelProvider) int {
		switch p.Group {
		case ClaudePoolGroup:
			if p.ID == ClaudePoolProvider {
				return 0
			}
		case ClaudeKeysGroup:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(providers, func(a, b ModelProvider) int { return rank(a) - rank(b) })
}

// newestPerFamily keeps the newest model of each Claude family, Opus,
// Sonnet, Fable and Haiku first, then other families by name. Hermes can
// list the pool's older pinned models too; IDs that name no Claude model
// stay after them, in order.
func newestPerFamily(models []ModelChoice) []ModelChoice {
	type best struct {
		version []int
		m       ModelChoice
	}
	newest := map[string]best{}
	var other []ModelChoice
	for _, m := range models {
		family, version, ok := claudeVersion(m.ID)
		if !ok {
			other = append(other, m)
			continue
		}
		if b, seen := newest[family]; !seen || slices.Compare(version, b.version) > 0 {
			newest[family] = best{version, m}
		}
	}
	families := make([]string, 0, len(newest))
	for f := range newest {
		families = append(families, f)
	}
	order := []string{"opus", "sonnet", "fable", "haiku"}
	rank := func(f string) int {
		if i := slices.Index(order, f); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortFunc(families, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	out := make([]ModelChoice, 0, len(families)+len(other))
	for _, f := range families {
		out = append(out, newest[f].m)
	}
	return append(out, other...)
}

// claudeVersion splits a Claude model ID into its family and version:
// "opus", [5 5] for claude-opus-5-5[1m].
func claudeVersion(id string) (string, []int, bool) {
	if _, ok := ClaudeModelName(id); !ok {
		return "", nil, false
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(id, "[1m]"), "claude-"), "-")
	var version []int
	for _, p := range parts[1:] {
		if n, err := strconv.Atoi(p); err == nil && len(p) != 8 {
			version = append(version, n)
		}
	}
	return parts[0], version, true
}

func labelModels(providers []ModelProvider) {
	for i := range providers {
		p := &providers[i]
		account, credits := strings.CutPrefix(p.ID, ClaudeKeysPrefix)
		switch {
		case p.ID == ClaudePoolProvider:
			p.Group = ClaudePoolGroup
		case credits && account != "":
			p.Group = ClaudeKeysGroup
		default:
			p.Group = p.Name
		}
		for j := range p.Models {
			m := &p.Models[j]
			m.Label = m.ID
			name, ok := ClaudeModelName(m.ID)
			switch {
			case !ok:
			case p.ID == ClaudePoolProvider:
				m.Label = name
			case credits && account != "":
				m.Label = name + " (" + account + " credits)"
			}
		}
	}
}

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
	LastEmail   string `json:"last_email,omitempty"`
	Plan        string `json:"plan,omitempty"`
	StatusError string `json:"status_error,omitempty"`
	// NoSignIn is an account that Hermes signs in to by itself, such as
	// its ChatGPT plan; the hub cannot sign it in.
	NoSignIn bool         `json:"no_signin,omitempty"`
	Usage    *ClaudeUsage `json:"usage,omitempty"`
	// Credits is the API credit estimate of the account's API key, with the
	// usage, when Hermes has a key for the account.
	Credits *ClaudeCredits `json:"credits,omitempty"`
	// SignIn is the running or last sign-in, for admins only.
	SignIn *ClaudeSignIn `json:"signin,omitempty"`
}

// ClaudeCredits estimates the API credits of a Claude account's API key:
// the credit its plan includes each month, less what Hermes spent through
// the key since the cycle started. Anthropic has no API that returns the
// balance, so it counts only Hermes's own use. MonthlyUSD and LeftUSD are
// nil when the plan's credit is unknown; SpentUSD is nil when Hermes's use
// could not be read.
type ClaudeCredits struct {
	Provider   string    `json:"provider"`
	MonthlyUSD *float64  `json:"monthly_usd"`
	SpentUSD   *float64  `json:"spent_usd"`
	LeftUSD    *float64  `json:"left_usd"`
	Since      time.Time `json:"since,omitzero"`
	ResetsAt   time.Time `json:"resets_at,omitzero"`
	// ResetsText is ResetsAt's day in the backend's time zone, "Dec 1".
	ResetsText string `json:"resets_text,omitempty"`
	// Unpriced counts calls whose cost is unknown, left out of SpentUSD.
	Unpriced int    `json:"unpriced,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ClaudePool says which account the Claude pool runs a chat's next turn
// on. Account is empty when no account can, and Message says why.
type ClaudePool struct {
	Account  string `json:"account,omitempty"`
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Pinned is true when the chat already runs on the account.
	Pinned bool `json:"pinned"`
	// ResetsAt is when the account's weekly window resets; ResetsText is
	// it in the backend's time zone, "Mon 12:00 AM CST".
	ResetsAt   time.Time `json:"resets_at,omitzero"`
	ResetsText string    `json:"resets_text,omitempty"`
	Message    string    `json:"message,omitempty"`
}

// ClaudePoolUsage is the Claude pool's weekly use as one number: the
// average of the "Current week" share used over the signed-in pool
// accounts, each capped at 100. Accounts is how many accounts the pool
// has and Read how many of them gave a weekly share; the rest are left out.
// UsedPercent is nil when Read is 0. RoomAccounts is the accounts' worth of
// room left. The Next fields say which full account's week resets first;
// they are empty when no account is full.
type ClaudePoolUsage struct {
	Accounts     int       `json:"accounts"`
	Read         int       `json:"read"`
	UsedPercent  *float64  `json:"used_percent"`
	RoomAccounts float64   `json:"room_accounts"`
	NextAccount  string    `json:"next_account,omitempty"`
	NextAt       time.Time `json:"next_at,omitzero"`
	NextText     string    `json:"next_text,omitempty"`
}

// ClaudeAccounts are a backend's Claude accounts. Admin says whether the
// user may sign the accounts in.
type ClaudeAccounts struct {
	BackendID string `json:"backend_id"`
	Admin     bool   `json:"admin"`
	// TimeZone is the IANA zone of the windows' ResetsText.
	TimeZone string          `json:"time_zone"`
	Accounts []ClaudeAccount `json:"accounts"`
	// Pool is the pool's weekly use, set when usage was asked for and the
	// backend has pool accounts.
	Pool *ClaudePoolUsage `json:"pool,omitempty"`
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
	// ClaudePool says which account the Claude pool runs the session's
	// next turn of model on.
	ClaudePool(ctx context.Context, user, backendID, sessionID, model string) (ClaudePool, error)
}

// Claude errors.
var (
	ErrNoClaude       = errors.New("this Hermes has no Claude accounts plugin")
	ErrUnknownAccount = errors.New("unknown Claude account")
	ErrNoSignIn       = errors.New("no sign-in is running for this account")
	ErrWrongSignIn    = errors.New("that sign-in is no longer the current one; start it again")
	ErrBadCode        = errors.New("that code was not accepted; copy the whole code from the Claude page")
)
