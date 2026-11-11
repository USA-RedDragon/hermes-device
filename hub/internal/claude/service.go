package claude

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

// LoggedOutHint starts the error that the claude-accounts plugin ends a
// turn with when the account it runs has no usable login. The plugin
// adds "(native: ...; cli: ...; login: <config dir>)".
const LoggedOutHint = "Claude Code is installed but has no usable login"

const (
	// cacheTTL is how long the hub keeps the account list and usage.
	cacheTTL = time.Minute
	// freshAfter is how old a usage read must be before a fresh read
	// (after a reply) goes to the plugin again.
	freshAfter = 15 * time.Second
	// missingTTL is how long the hub remembers a backend without the
	// plugin.
	missingTTL = 5 * time.Minute
	// failureQuiet is how long the hub ignores further failed turns of
	// one chat and account after it handled one.
	failureQuiet = time.Minute
	// noticeWindow is how long one notification per account lasts.
	noticeWindow = 30 * time.Minute
	// maxCode bounds the code a person pastes.
	maxCode = 4096
)

// Access says whether a user may use a backend. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Options are the dependencies of a Service.
type Options struct {
	Access Access
	// Admins are the Claude admins of each backend.
	Admins map[string][]string
	// Location is the time zone of reset times. Default UTC.
	Location *time.Location
	// Requesters reach each backend's dashboard with the hub's session.
	Requesters map[string]device.Requester
	// Notify raises a hub notification. Nil: none.
	Notify func(hub.Notice)
	// Announce sends an event to the viewers of a chat. Nil: none.
	Announce func(backendID, sessionID, kind string, data any) bool
	// Timeout bounds each plugin call; a sign-in call gets more. Default 20s.
	Timeout time.Duration
	Logger  *slog.Logger
	Now     func() time.Time
}

type accountsEntry struct {
	at       time.Time
	usage    bool
	accounts Accounts
	missing  bool
}

type usageEntry struct {
	at    time.Time
	usage Usage
}

// Service implements hub.Claude.
type Service struct {
	o       Options
	log     *slog.Logger
	clients map[string]*Client
	admins  map[string][]string

	mu       sync.Mutex
	accounts map[string]accountsEntry
	usage    map[string]usageEntry
	handled  map[string]time.Time
}

var _ hub.Claude = (*Service)(nil)

// New returns a Service.
func New(o Options) *Service {
	if o.Timeout == 0 {
		o.Timeout = 20 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	s := &Service{o: o, log: o.Logger.With("component", "claude"), clients: map[string]*Client{}, admins: map[string][]string{},
		accounts: map[string]accountsEntry{}, usage: map[string]usageEntry{},
		handled: map[string]time.Time{}}
	for id, r := range o.Requesters {
		if r != nil {
			s.clients[id] = NewClient(r)
		}
	}
	for id, users := range o.Admins {
		s.admins[id] = slices.Clone(users)
	}
	return s
}

// IsAdmin reports whether user may sign a backend's Claude accounts in.
func (s *Service) IsAdmin(user, backendID string) bool {
	return slices.Contains(s.admins[backendID], user)
}

// Admin reports whether user is a Claude admin of any backend.
func (s *Service) Admin(user string) bool {
	for id := range s.admins {
		if s.IsAdmin(user, id) {
			return true
		}
	}
	return false
}

func (s *Service) client(ctx context.Context, user, backendID string) (*Client, error) {
	if err := s.o.Access.Allowed(ctx, user, backendID); err != nil {
		return nil, err
	}
	c := s.clients[backendID]
	if c == nil {
		return nil, hub.ErrNoClaude
	}
	return c, nil
}

func (s *Service) adminClient(ctx context.Context, user, backendID string) (*Client, error) {
	c, err := s.client(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	if !s.IsAdmin(user, backendID) {
		return nil, hub.ErrForbidden
	}
	return c, nil
}

// mapError turns a plugin error into a hub error. Errors never carry what
// the hub sent, so they are safe to log.
func mapError(err error) error {
	var pe *Error
	if !errors.As(err, &pe) {
		return err
	}
	switch {
	case pe.Code == CodeUnknownAccount:
		return hub.ErrUnknownAccount
	case pe.Code == CodeNotFound:
		return hub.ErrNoSignIn
	case pe.Code == CodeWrongSignIn:
		return hub.ErrWrongSignIn
	case pe.Code == CodeBadCode:
		return hub.ErrBadCode
	case pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed:
		return hub.ErrNoClaude
	}
	return err
}

// list returns a backend's accounts from the cache or the plugin.
func (s *Service) list(ctx context.Context, backendID string, c *Client, usage bool) (Accounts, error) {
	now := s.o.Now()
	s.mu.Lock()
	e, ok := s.accounts[backendID]
	s.mu.Unlock()
	if ok && e.missing && now.Sub(e.at) < missingTTL {
		return Accounts{}, hub.ErrNoClaude
	}
	if ok && !e.missing && now.Sub(e.at) < cacheTTL && (e.usage || !usage) {
		return e.accounts, nil
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	a, err := c.Accounts(cctx, usage)
	if err = mapError(err); err != nil {
		if errors.Is(err, hub.ErrNoClaude) {
			s.mu.Lock()
			s.accounts[backendID] = accountsEntry{at: now, missing: true}
			s.mu.Unlock()
		}
		return Accounts{}, err
	}
	s.mu.Lock()
	s.accounts[backendID] = accountsEntry{at: now, usage: usage, accounts: a}
	if usage {
		for _, x := range a.Accounts {
			if x.Usage != nil {
				s.usage[backendID+"/"+x.ID] = usageEntry{at: now, usage: *x.Usage}
			}
		}
	}
	s.mu.Unlock()
	return a, nil
}

// forget drops what the hub keeps about a backend's accounts.
func (s *Service) forget(backendID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, backendID)
	delete(s.usage, backendID+"/"+accountID)
}

// ClaudeAccounts lists a backend's Claude accounts. See hub.Claude.
func (s *Service) ClaudeAccounts(ctx context.Context, user, backendID string, usage bool) (hub.ClaudeAccounts, error) {
	c, err := s.client(ctx, user, backendID)
	if err != nil {
		return hub.ClaudeAccounts{}, err
	}
	a, err := s.list(ctx, backendID, c, usage)
	if err != nil {
		return hub.ClaudeAccounts{}, err
	}
	admin := s.IsAdmin(user, backendID)
	out := hub.ClaudeAccounts{BackendID: backendID, Admin: admin, TimeZone: s.o.Location.String(), Accounts: make([]hub.ClaudeAccount, 0, len(a.Accounts))}
	for _, x := range a.Accounts {
		h := hub.ClaudeAccount{ID: x.ID, Label: x.Label, Provider: x.Provider, SignedIn: x.Status.LoggedIn,
			Email: x.Status.Email, LastEmail: x.LastEmail, Plan: planName(x.Status.SubscriptionType), StatusError: x.Status.Error}
		if h.Email == "" {
			h.Email = x.LastEmail
		}
		if usage && x.Usage != nil {
			u := s.convertUsage(*x.Usage)
			h.Usage = &u
		}
		if usage && x.Credits != nil {
			h.Credits = s.convertCredits(*x.Credits)
		}
		if admin && x.SignIn != nil {
			si := convertSignIn(*x.SignIn)
			si.URL = ""
			h.SignIn = &si
		}
		out.Accounts = append(out.Accounts, h)
	}
	return out, nil
}

// ClaudeUsage reads one account's usage. See hub.Claude.
func (s *Service) ClaudeUsage(ctx context.Context, user, backendID, accountID string, fresh bool) (hub.ClaudeUsage, error) {
	c, err := s.client(ctx, user, backendID)
	if err != nil {
		return hub.ClaudeUsage{}, err
	}
	key := backendID + "/" + accountID
	now := s.o.Now()
	s.mu.Lock()
	e, ok := s.usage[key]
	missing := s.accounts[backendID].missing && now.Sub(s.accounts[backendID].at) < missingTTL
	s.mu.Unlock()
	if missing {
		return hub.ClaudeUsage{}, hub.ErrNoClaude
	}
	if ok && (now.Sub(e.at) < freshAfter || (!fresh && now.Sub(e.at) < cacheTTL)) {
		return s.convertUsage(e.usage), nil
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	u, err := c.Usage(cctx, accountID)
	if err = mapError(err); err != nil {
		return hub.ClaudeUsage{}, err
	}
	s.mu.Lock()
	s.usage[key] = usageEntry{at: now, usage: u}
	s.mu.Unlock()
	return s.convertUsage(u), nil
}

// StartSignIn starts a sign-in, or returns the running one. See hub.Claude.
func (s *Service) StartSignIn(ctx context.Context, user, backendID, accountID string) (hub.ClaudeSignIn, error) {
	c, err := s.adminClient(ctx, user, backendID)
	if err != nil {
		return hub.ClaudeSignIn{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout+30*time.Second)
	defer cancel()
	si, err := c.StartSignIn(cctx, accountID)
	if err = mapError(err); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	s.log.Info("Claude sign-in started", "backend", backendID, "account", accountID, "user", user, "state", si.State)
	return convertSignIn(si), nil
}

// SignInState returns an account's running or last sign-in. See hub.Claude.
func (s *Service) SignInState(ctx context.Context, user, backendID, accountID string) (hub.ClaudeSignIn, error) {
	c, err := s.adminClient(ctx, user, backendID)
	if err != nil {
		return hub.ClaudeSignIn{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	si, ok, err := c.SignIn(cctx, accountID)
	if err = mapError(err); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	if !ok {
		return hub.ClaudeSignIn{}, hub.ErrNoSignIn
	}
	out := convertSignIn(si)
	if out.State == hub.SignInSucceeded {
		// The sign-in can end after the code was sent, and the account
		// list fetched since then still says signed out.
		s.forget(backendID, accountID)
	}
	return out, nil
}

// validCode reports whether code can be a code from the Claude sign-in
// page: no spaces or control characters, and not too long.
func validCode(code string) bool {
	if code == "" || len(code) > maxCode {
		return false
	}
	for _, r := range code {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// SendSignInCode passes the code to the plugin. The code goes nowhere
// else: not into logs, errors or the hub's store. See hub.Claude.
func (s *Service) SendSignInCode(ctx context.Context, user, backendID, accountID, signInID, code string) (hub.ClaudeSignIn, error) {
	c, err := s.adminClient(ctx, user, backendID)
	if err != nil {
		return hub.ClaudeSignIn{}, err
	}
	code = strings.TrimSpace(code)
	if !validCode(code) {
		return hub.ClaudeSignIn{}, hub.ErrBadCode
	}
	if signInID == "" {
		return hub.ClaudeSignIn{}, hub.ErrWrongSignIn
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	si, err := c.SendCode(cctx, accountID, signInID, code)
	if err = mapError(err); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	s.forget(backendID, accountID)
	s.log.Info("Claude sign-in code sent", "backend", backendID, "account", accountID, "user", user, "state", si.State)
	return convertSignIn(si), nil
}

// CancelSignIn stops an account's sign-in. See hub.Claude.
func (s *Service) CancelSignIn(ctx context.Context, user, backendID, accountID string) (hub.ClaudeSignIn, error) {
	c, err := s.adminClient(ctx, user, backendID)
	if err != nil {
		return hub.ClaudeSignIn{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	si, err := c.CancelSignIn(cctx, accountID)
	if err = mapError(err); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	s.log.Info("Claude sign-in cancelled", "backend", backendID, "account", accountID, "user", user)
	return convertSignIn(si), nil
}

// ClaudePool says which account the pool runs a session's next turn on.
// See hub.Claude.
func (s *Service) ClaudePool(ctx context.Context, user, backendID, sessionID, model string) (hub.ClaudePool, error) {
	c, err := s.client(ctx, user, backendID)
	if err != nil {
		return hub.ClaudePool{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	p, err := c.Pool(cctx, sessionID, model)
	if err = mapError(err); err != nil {
		return hub.ClaudePool{}, err
	}
	out := hub.ClaudePool{Account: p.Account, Label: p.Label, Provider: p.Provider, Pinned: p.Pinned,
		ResetsAt: p.WeeklyResetsAt, Message: p.Message}
	if !p.WeeklyResetsAt.IsZero() {
		out.ResetsText = ResetText(p.WeeklyResetsAt, s.o.Now(), s.o.Location)
	}
	if out.Account == "" && out.Message == "" {
		out.Message = "No Claude account can run this chat now."
	}
	return out, nil
}

// convertCredits copies the plugin's API credit estimate, with the reset
// day in the backend's time zone.
func (s *Service) convertCredits(x Credits) *hub.ClaudeCredits {
	out := &hub.ClaudeCredits{Provider: x.Provider, MonthlyUSD: x.MonthlyUSD, SpentUSD: x.SpentUSD, LeftUSD: x.LeftUSD,
		Since: x.Since, ResetsAt: x.ResetsAt, Unpriced: x.UnpricedCalls, Error: x.Error}
	if !x.ResetsAt.IsZero() {
		out.ResetsText = x.ResetsAt.In(s.o.Location).Format("Jan 2")
	}
	return out
}

func convertSignIn(x SignIn) hub.ClaudeSignIn {
	return hub.ClaudeSignIn{ID: x.ID, Account: x.Account, Label: x.Label, EmailHint: x.EmailHint, Reason: x.Reason,
		State: x.State, URL: x.URL, Message: x.Message, Email: x.Email, StartedAt: x.StartedAt, ExpiresAt: x.ExpiresAt}
}

// planName names a subscription type as Anthropic does.
func planName(t string) string {
	switch strings.ToLower(t) {
	case "":
		return ""
	case "pro":
		return "Pro"
	case "max":
		return "Max"
	case "team":
		return "Team"
	case "enterprise":
		return "Enterprise"
	}
	return t
}

// convertUsage keeps only the windows the account's CLI reports a share
// of: some plans have no 5-hour window, and it is left out rather than
// shown empty.
func (s *Service) convertUsage(u Usage) hub.ClaudeUsage {
	out := hub.ClaudeUsage{Windows: make([]hub.ClaudeWindow, 0, len(u.Windows)), Details: u.Details,
		Unavailable: u.UnavailableReason, FetchedAt: u.FetchedAt}
	now := s.o.Now()
	for _, w := range u.Windows {
		if w.UsedPercent == nil {
			continue
		}
		cw := hub.ClaudeWindow{Kind: w.Kind, Label: w.Label, UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt}
		if !w.ResetsAt.IsZero() {
			cw.ResetsText = ResetText(w.ResetsAt, now, s.o.Location)
		}
		out.Windows = append(out.Windows, cw)
	}
	return out
}

// ResetText is t in loc as a person reads a reset time: "3:40 PM CST"
// on the same day as now, "Thu 3:40 PM CST" within the next six days,
// else "Nov 20, 3:40 PM CST".
func ResetText(t, now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	t, now = t.In(loc), now.In(loc)
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	today := time.Date(ny, nm, nd, 0, 0, 0, 0, loc)
	day := time.Date(ty, tm, td, 0, 0, 0, 0, loc)
	switch {
	case day.Equal(today):
		return t.Format("3:04 PM MST")
	case day.After(today) && day.Before(today.AddDate(0, 0, 7)):
		return t.Format("Mon 3:04 PM MST")
	}
	return t.Format("Jan 2, 3:04 PM MST")
}

// Describe names an account and says why it needs a sign-in, for every
// sign-in message: the label, the email it was last signed in to when
// known, and the reason.
func Describe(label, emailHint, reason string) string {
	var b strings.Builder
	b.WriteString(label)
	if emailHint != "" {
		b.WriteString(" (last signed in as " + emailHint + ")")
	}
	if reason == hub.SignInFirst {
		b.WriteString(" needs a sign-in: this is its first sign-in.")
	} else {
		b.WriteString(" needs a sign-in: the login expired or was revoked.")
	}
	return b.String()
}

// authFailure reports whether a failed turn's text says the Claude login
// is gone, and the config dir the plugin named, if any.
func authFailure(provider, text string) (string, bool) {
	if i := strings.Index(text, LoggedOutHint); i >= 0 {
		rest := text[i:]
		dir := ""
		if j := strings.Index(rest, "login: "); j >= 0 {
			dir = rest[j+len("login: "):]
			if k := strings.IndexAny(dir, ")\n"); k >= 0 {
				dir = dir[:k]
			}
			dir = strings.TrimSpace(dir)
		}
		return dir, true
	}
	if strings.HasPrefix(provider, "claude-") && (strings.Contains(text, "status 401") || strings.Contains(text, "authentication_failed")) {
		return "", true
	}
	return "", false
}

// TurnFailed handles a failed turn: when its text says the chat's Claude
// account lost its login, and the plugin confirms that the account is
// signed out, it shows a notice in the chat and notifies the backend's
// Claude admins.
func (s *Service) TurnFailed(e service.TurnError) {
	dir, ok := authFailure(e.Provider, e.Text)
	if !ok {
		return
	}
	c := s.clients[e.BackendID]
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*s.o.Timeout)
	defer cancel()
	a, err := s.list(ctx, e.BackendID, c, false)
	if err != nil {
		s.log.Info("Claude login check: list accounts", "backend", e.BackendID, "error", err.Error())
		return
	}
	var acc *Account
	for i, x := range a.Accounts {
		if (e.Provider != "" && x.Provider == e.Provider) || (dir != "" && x.ConfigDir == dir) {
			acc = &a.Accounts[i]
			break
		}
	}
	if acc == nil {
		return
	}
	key := e.BackendID + "/" + acc.ID + "/" + e.SessionID
	now := s.o.Now()
	s.mu.Lock()
	if t, ok := s.handled[key]; ok && now.Sub(t) < failureQuiet {
		s.mu.Unlock()
		return
	}
	s.handled[key] = now
	for k, t := range s.handled {
		if now.Sub(t) >= failureQuiet {
			delete(s.handled, k)
		}
	}
	s.mu.Unlock()
	st, err := c.Status(ctx, acc.ID)
	if err != nil {
		s.log.Info("Claude login check: status", "backend", e.BackendID, "account", acc.ID, "error", err.Error())
		return
	}
	if st.Status.LoggedIn {
		return
	}
	s.forget(e.BackendID, acc.ID)
	hint := st.Status.Email
	if hint == "" {
		hint = st.LastEmail
	}
	if hint == "" {
		hint = acc.LastEmail
	}
	reason := hub.SignInExpired
	if hint == "" {
		reason = hub.SignInFirst
	}
	label := st.Label
	if label == "" {
		label = acc.Label
	}
	s.log.Info("Claude account needs a sign-in", "backend", e.BackendID, "account", acc.ID, "session", e.SessionID)
	if s.o.Announce != nil {
		s.o.Announce(e.BackendID, e.SessionID, hub.KindClaudeSignIn, hub.ClaudeSignInNotice{Account: acc.ID, Label: label,
			Provider: acc.Provider, EmailHint: hint, Reason: reason})
	}
	if s.o.Notify != nil && len(s.admins[e.BackendID]) > 0 {
		s.o.Notify(hub.Notice{Kind: hub.NoticeClaudeSignIn, BackendID: e.BackendID, SessionID: e.SessionID,
			Key:     "claude:" + e.BackendID + ":" + acc.ID + ":" + strconv.FormatInt(now.Unix()/int64(noticeWindow/time.Second), 10),
			Title:   e.Title,
			Detail:  Describe(label, hint, reason) + " Open the hub to sign in.",
			Users:   slices.Clone(s.admins[e.BackendID]),
			Account: acc.ID})
	}
}
