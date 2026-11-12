package claude_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/claude"
	"github.com/USA-RedDragon/hermes-hub/internal/claude/claudetest"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	alice       = "alice"
	bob         = "bob"
	backendMain = "main"
	other       = "other"
	solo        = "solo"
	account     = "account1"
	aliceEmail  = "alice@example.com"
	provider1   = "claude-account1"
)

// access lets alice and bob use backendMain, and only bob use other.
type access struct{}

func (access) Allowed(_ context.Context, user, id string) error {
	if id == backendMain && (user == alice || user == bob) || (id == other || id == solo) && user == bob {
		return nil
	}
	return hub.ErrForbidden
}

type recorder struct {
	mu        sync.Mutex
	notices   []hub.Notice
	announced []hub.ClaudeSignInNotice
	sessions  []string
}

func (r *recorder) notify(n hub.Notice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, n)
}

func (r *recorder) announce(_, sessionID, kind string, data any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kind == hub.KindClaudeSignIn {
		n, _ := data.(hub.ClaudeSignInNotice)
		r.announced = append(r.announced, n)
		r.sessions = append(r.sessions, sessionID)
	}
	return true
}

type env struct {
	plugin *claudetest.Plugin
	// chatgpt is backendMain's model options; soloGPT is those of solo,
	// a Hermes with a ChatGPT plan and no plugin.
	chatgpt, soloGPT *claudetest.ChatGPT
	svc              *claude.Service
	rec              *recorder
	logs             *bytes.Buffer
	now              time.Time
}

// newEnv runs a fake plugin on backendMain; other has a Hermes without it.
func newEnv(t *testing.T) *env {
	t.Helper()
	gw := gatewaytest.New("hub", "pw")
	t.Cleanup(gw.Close)
	bare := gatewaytest.New("hub", "pw")
	t.Cleanup(bare.Close)
	sgw := gatewaytest.New("hub", "pw")
	t.Cleanup(sgw.Close)
	e := &env{plugin: claudetest.New(gw), chatgpt: claudetest.NewChatGPT(gw), soloGPT: claudetest.NewChatGPT(sgw), rec: &recorder{}, logs: &bytes.Buffer{},
		now: time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	creds := func(u string) device.Requester {
		return gateway.NewDialer(gateway.Credentials{BaseURL: u, Provider: "basic", Username: "hub", Password: "pw"})
	}
	e.svc = claude.New(claude.Options{Access: access{}, Admins: map[string][]string{backendMain: {alice}}, Location: loc,
		Requesters: map[string]device.Requester{backendMain: creds(gw.URL()), other: creds(bare.URL()), solo: creds(sgw.URL())},
		UsageRetry: 10 * time.Millisecond,
		Notify:     e.rec.notify, Announce: e.rec.announce, Now: func() time.Time { return e.now },
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	return e
}

func pct(f float64) *float64 { return &f }

func TestAccountsAndUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.plugin.SetLoggedIn(account, true, aliceEmail)
	e.plugin.Lock()
	e.plugin.Usages[account] = claude.Usage{Windows: []claude.Window{
		{Kind: "session", Label: "Current session", UsedPercent: pct(12), ResetsAt: time.Date(2026, 11, 11, 21, 40, 0, 0, time.UTC)},
		{Kind: "weekly_all", Label: "Current week", UsedPercent: pct(40), ResetsAt: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{Kind: "weekly_opus", Label: "Opus week"}}}
	e.plugin.Unlock()

	got, err := e.svc.ClaudeAccounts(ctx, bob, backendMain, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Admin || len(got.Accounts) != 2 || got.TimeZone != "America/Chicago" {
		t.Fatalf("accounts = %+v", got)
	}
	a := got.Accounts[0]
	if !a.SignedIn || a.Email != aliceEmail || a.Provider != provider1 || a.Usage == nil || len(a.Usage.Windows) != 2 {
		t.Fatalf("account = %+v", a)
	}
	if w := a.Usage.Windows[0]; w.ResetsText != "3:40 PM CST" || *w.UsedPercent != 12 {
		t.Errorf("session window = %+v", w)
	}
	if w := a.Usage.Windows[1]; w.ResetsText != "Fri 9:00 AM CST" {
		t.Errorf("weekly window = %+v", w)
	}
	for _, w := range a.Usage.Windows {
		if w.Kind == "weekly_opus" {
			t.Errorf("a window the CLI reports no share of is shown: %+v", w)
		}
	}

	if _, err := e.svc.ClaudeAccounts(ctx, alice, backendMain, true); err != nil {
		t.Fatal(err)
	}
	if n := e.plugin.CallCount("GET /accounts"); n != 1 {
		t.Errorf("plugin got %d account lists, want 1 (cached)", n)
	}
	if _, err := e.svc.ClaudeUsage(ctx, alice, backendMain, account, false); err != nil {
		t.Fatal(err)
	}
	if n := e.plugin.CallCount("GET /accounts/account1/usage"); n != 0 {
		t.Errorf("usage read %d times, want the cached one", n)
	}
	e.now = e.now.Add(20 * time.Second)
	if _, err := e.svc.ClaudeUsage(ctx, alice, backendMain, account, true); err != nil {
		t.Fatal(err)
	}
	if n := e.plugin.CallCount("GET /accounts/account1/usage"); n != 1 {
		t.Errorf("fresh usage read %d times, want 1", n)
	}
	if _, err := e.svc.ClaudeUsage(ctx, alice, backendMain, "nope", true); !errors.Is(err, hub.ErrUnknownAccount) {
		t.Errorf("unknown account: %v", err)
	}
	if _, err := e.svc.ClaudeAccounts(ctx, alice, other, false); !errors.Is(err, hub.ErrForbidden) {
		t.Errorf("alice on other: %v", err)
	}
	if _, err := e.svc.ClaudeAccounts(ctx, bob, other, false); !errors.Is(err, hub.ErrNoClaude) {
		t.Errorf("backend without plugin: %v", err)
	}
}

func codexRow(session, week float64) *claude.ProviderRow {
	yes := true
	return &claude.ProviderRow{Slug: claude.ChatGPTProvider, Authenticated: &yes, Usage: &claude.ProviderUsage{Windows: []claude.ProviderWindow{
		{Label: "Session", UsedPercent: pct(session), ResetsAt: time.Date(2026, 11, 11, 21, 40, 0, 0, time.UTC)},
		{Label: "Weekly", UsedPercent: pct(week), ResetsAt: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)}}}}
}

func TestChatGPTUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.chatgpt.Lock()
	e.chatgpt.Row, e.chatgpt.Cold = codexRow(12, 2), 1
	e.chatgpt.Unlock()

	got, err := e.svc.ClaudeAccounts(ctx, alice, backendMain, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 3 {
		t.Fatalf("accounts = %+v, want the two Claude accounts and ChatGPT", got.Accounts)
	}
	a := got.Accounts[2]
	if a.ID != claude.ChatGPTAccount || a.Label != "ChatGPT" || a.Provider != "openai-codex" || !a.SignedIn || !a.NoSignIn || a.SignIn != nil ||
		a.Usage == nil || a.Usage.Unavailable != "" || len(a.Usage.Windows) != 2 {
		t.Fatalf("ChatGPT account = %+v", a)
	}
	if w := a.Usage.Windows[0]; w.Kind != hub.ClaudeWindowSession || w.Label != "Current session" || *w.UsedPercent != 12 || w.ResetsText != "3:40 PM CST" {
		t.Errorf("session window = %+v", w)
	}
	if w := a.Usage.Windows[1]; w.Kind != hub.ClaudeWindowWeekly || w.Label != "Current week" || *w.UsedPercent != 2 || w.ResetsText != "Fri 9:00 AM CST" {
		t.Errorf("weekly window = %+v", w)
	}
	if n := e.chatgpt.CallCount(); n != 2 {
		t.Errorf("model options read %d times, want 2 (once more after the cold answer)", n)
	}

	e.chatgpt.Lock()
	e.chatgpt.Row = codexRow(30, 5)
	e.chatgpt.Unlock()
	u, err := e.svc.ClaudeUsage(ctx, bob, backendMain, claude.ChatGPTAccount, false)
	if err != nil || *u.Windows[0].UsedPercent != 12 {
		t.Fatalf("cached usage = %+v %v", u, err)
	}
	e.now = e.now.Add(20 * time.Second)
	u, err = e.svc.ClaudeUsage(ctx, bob, backendMain, claude.ChatGPTAccount, true)
	if err != nil || *u.Windows[0].UsedPercent != 30 || e.chatgpt.CallCount() != 3 {
		t.Fatalf("fresh usage = %+v %v after %d reads", u, err, e.chatgpt.CallCount())
	}
	if _, err := e.svc.StartSignIn(ctx, alice, backendMain, claude.ChatGPTAccount); !errors.Is(err, hub.ErrUnknownAccount) {
		t.Errorf("ChatGPT sign-in: %v", err)
	}
}

func TestChatGPTWithoutPlugin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.soloGPT.Lock()
	e.soloGPT.Row, e.soloGPT.Cold = codexRow(50, 60), 5
	e.soloGPT.Unlock()
	got, err := e.svc.ClaudeAccounts(ctx, bob, solo, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Usage == nil || got.Accounts[0].Usage.Unavailable == "" || len(got.Accounts[0].Usage.Windows) != 0 {
		t.Fatalf("Hermes without the plugin and a ChatGPT plan not read yet = %+v", got.Accounts)
	}
	if _, err := e.svc.ClaudeUsage(ctx, bob, other, claude.ChatGPTAccount, false); !errors.Is(err, hub.ErrUnknownAccount) {
		t.Errorf("ChatGPT usage on a Hermes without model options: %v", err)
	}
	if _, err := e.svc.ClaudeAccounts(ctx, bob, other, true); !errors.Is(err, hub.ErrNoClaude) {
		t.Errorf("Hermes without the plugin or ChatGPT: %v", err)
	}
}

func TestSignInOnlyForAdmins(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	for name, f := range map[string]func() error{
		"start": func() error { _, err := e.svc.StartSignIn(ctx, bob, backendMain, account); return err },
		"state": func() error { _, err := e.svc.SignInState(ctx, bob, backendMain, account); return err },
		"code": func() error {
			_, err := e.svc.SendSignInCode(ctx, bob, backendMain, account, "si1", claudetest.GoodCode)
			return err
		},
		"cancel": func() error { _, err := e.svc.CancelSignIn(ctx, bob, backendMain, account); return err },
	} {
		if err := f(); !errors.Is(err, hub.ErrForbidden) {
			t.Errorf("%s by a non-admin: %v", name, err)
		}
	}
	if n := e.plugin.CallCount("POST"); n != 0 {
		t.Errorf("plugin got %d POSTs from a non-admin", n)
	}
	got, err := e.svc.ClaudeAccounts(ctx, bob, backendMain, false)
	if err != nil || got.Admin {
		t.Fatalf("bob: %+v %v", got, err)
	}
}

func TestSignInFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.SignInState(ctx, alice, backendMain, account); !errors.Is(err, hub.ErrNoSignIn) {
		t.Fatalf("no sign-in yet: %v", err)
	}
	si, err := e.svc.StartSignIn(ctx, alice, backendMain, account)
	if err != nil {
		t.Fatal(err)
	}
	if si.State != hub.SignInWaiting || !strings.HasPrefix(si.URL, "https://claude.com/") || si.Label != "Claude (account1)" || si.Reason != hub.SignInFirst {
		t.Fatalf("sign-in = %+v", si)
	}
	again, err := e.svc.StartSignIn(ctx, alice, backendMain, account)
	if err != nil || again.ID != si.ID {
		t.Fatalf("second start = %+v %v, want the running one", again, err)
	}
	list, err := e.svc.ClaudeAccounts(ctx, alice, backendMain, false)
	if err != nil || !list.Admin || list.Accounts[0].SignIn == nil || list.Accounts[0].SignIn.URL != "" {
		t.Fatalf("admin list = %+v %v, want the sign-in without its URL", list, err)
	}

	if _, err := e.svc.SendSignInCode(ctx, alice, backendMain, account, si.ID, "has space#x"); !errors.Is(err, hub.ErrBadCode) {
		t.Errorf("code with a space: %v", err)
	}
	if _, err := e.svc.SendSignInCode(ctx, alice, backendMain, account, "stale", claudetest.GoodCode); !errors.Is(err, hub.ErrWrongSignIn) {
		t.Errorf("stale sign-in: %v", err)
	}
	if _, err := e.svc.SendSignInCode(ctx, alice, backendMain, account, si.ID, "nohash"); !errors.Is(err, hub.ErrBadCode) {
		t.Errorf("plugin bad_code: %v", err)
	}
	done, err := e.svc.SendSignInCode(ctx, alice, backendMain, account, si.ID, "  "+claudetest.GoodCode+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != hub.SignInSucceeded || done.Email != aliceEmail {
		t.Fatalf("after the code: %+v", done)
	}
	e.plugin.Lock()
	sent := e.plugin.Codes[si.ID]
	e.plugin.Unlock()
	if sent != claudetest.GoodCode {
		t.Errorf("plugin got code %q", sent)
	}
	list, err = e.svc.ClaudeAccounts(ctx, alice, backendMain, false)
	if err != nil || !list.Accounts[0].SignedIn || list.Accounts[0].Plan != "Max" {
		t.Errorf("after sign-in = %+v %v (want a fresh read)", list, err)
	}

	logs := e.logs.String()
	for _, secret := range []string{claudetest.GoodCode, "good-code", "nohash", "oauth/authorize"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs hold %q:\n%s", secret, logs)
		}
	}
}

func TestCancelSignIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.StartSignIn(ctx, alice, backendMain, account); err != nil {
		t.Fatal(err)
	}
	si, err := e.svc.CancelSignIn(ctx, alice, backendMain, account)
	if err != nil || si.State != hub.SignInCancelled {
		t.Fatalf("cancel = %+v %v", si, err)
	}
}

func TestTurnFailedLoggedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.plugin.SetLoggedIn(account, true, aliceEmail)
	e.plugin.SetLoggedIn(account, false, "")
	text := "Error: " + claude.LoggedOutHint + " (native: no; cli: no; login: /opt/data/hermes/claude/account1)"
	e.svc.TurnFailed(service.TurnError{BackendID: backendMain, SessionID: "s1", Title: "Plans", Provider: "", Text: text})
	e.svc.TurnFailed(service.TurnError{BackendID: backendMain, SessionID: "s1", Title: "Plans", Provider: "", Text: text})
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	if len(e.rec.announced) != 1 || len(e.rec.notices) != 1 {
		t.Fatalf("announced %d, notified %d, want 1 each", len(e.rec.announced), len(e.rec.notices))
	}
	a := e.rec.announced[0]
	if a.Account != account || a.EmailHint != aliceEmail || a.Reason != hub.SignInExpired || e.rec.sessions[0] != "s1" {
		t.Errorf("announced %+v", a)
	}
	n := e.rec.notices[0]
	if n.Kind != hub.NoticeClaudeSignIn || len(n.Users) != 1 || n.Users[0] != alice || n.SessionID != "s1" ||
		n.Detail != "Claude (account1) (last signed in as alice@example.com) needs a sign-in: the login expired or was revoked. Open the hub to sign in." {
		t.Errorf("notice %+v", n)
	}
	if strings.Contains(n.Detail, "http") {
		t.Errorf("notice holds a link: %q", n.Detail)
	}
}

func TestTurnFailedIgnored(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.plugin.SetLoggedIn("account2", true, "bob@example.com")
	for _, te := range []service.TurnError{
		{BackendID: backendMain, SessionID: "s", Provider: "claude-account2", Text: "upstream status 401"},
		{BackendID: backendMain, SessionID: "s", Provider: "ollama", Text: "status 401"},
		{BackendID: backendMain, SessionID: "s", Provider: provider1, Text: "context too long"},
		{BackendID: other, SessionID: "s", Provider: provider1, Text: claude.LoggedOutHint},
	} {
		e.svc.TurnFailed(te)
	}
	if len(e.rec.announced)+len(e.rec.notices) != 0 {
		t.Errorf("announced %+v, notified %+v", e.rec.announced, e.rec.notices)
	}
	e.svc.TurnFailed(service.TurnError{BackendID: backendMain, SessionID: "s", Provider: provider1, Text: `{"type":"authentication_failed"}`})
	if len(e.rec.announced) != 1 || e.rec.announced[0].Reason != hub.SignInFirst {
		t.Errorf("revoked token: announced %+v", e.rec.announced)
	}
}

func TestResetText(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 7, 1, 17, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 7, 1, 20, 40, 0, 0, time.UTC), "3:40 PM CDT"},
		{time.Date(2026, 7, 2, 4, 0, 0, 0, time.UTC), "11:00 PM CDT"},
		{time.Date(2026, 7, 2, 6, 0, 0, 0, time.UTC), "Thu 1:00 AM CDT"},
		{time.Date(2026, 7, 20, 6, 0, 0, 0, time.UTC), "Jul 20, 1:00 AM CDT"},
	} {
		if got := claude.ResetText(c.at, now, loc); got != c.want {
			t.Errorf("ResetText(%v) = %q, want %q", c.at, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()
	if got := claude.Describe("Claude (account2)", "", hub.SignInFirst); got != "Claude (account2) needs a sign-in: this is its first sign-in." {
		t.Errorf("first: %q", got)
	}
}

func TestSignInEndedLaterRefreshesTheList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.StartSignIn(ctx, alice, backendMain, account); err != nil {
		t.Fatal(err)
	}
	if list, err := e.svc.ClaudeAccounts(ctx, alice, backendMain, false); err != nil || list.Accounts[0].SignedIn {
		t.Fatalf("before = %+v %v", list, err)
	}
	e.plugin.Lock()
	e.plugin.SignIns[account].State = hub.SignInSucceeded
	e.plugin.Unlock()
	e.plugin.SetLoggedIn(account, true, aliceEmail)
	if si, err := e.svc.SignInState(ctx, alice, backendMain, account); err != nil || si.State != hub.SignInSucceeded {
		t.Fatalf("state = %+v %v", si, err)
	}
	if list, err := e.svc.ClaudeAccounts(ctx, alice, backendMain, false); err != nil || !list.Accounts[0].SignedIn {
		t.Fatalf("after a sign-in that ended later = %+v %v (want a fresh read)", list, err)
	}
}

func TestPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.plugin.Lock()
	e.plugin.Pools["s1"] = claude.Pool{Account: "account2", Label: "Claude (account2)", Provider: "claude-account2", Pinned: true,
		WeeklyResetsAt: time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)}
	e.plugin.Pool = claude.Pool{Message: "All Claude accounts are at their limit; the first resets Mon 12:00 AM CST"}
	e.plugin.Unlock()

	got, err := e.svc.ClaudePool(ctx, bob, backendMain, "s1", "claude-opus-5-5[1m]")
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != "account2" || !got.Pinned || got.ResetsText != "Mon 12:00 AM CST" || got.Message != "" {
		t.Fatalf("pool = %+v", got)
	}
	if n := e.plugin.CallCount("GET /pool?model=claude-opus-5-5%5B1m%5D&session=s1"); n != 1 {
		t.Errorf("plugin calls = %v", e.plugin.Calls)
	}
	got, err = e.svc.ClaudePool(ctx, bob, backendMain, "s2", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != "" || !strings.Contains(got.Message, "first resets Mon 12:00 AM CST") {
		t.Fatalf("exhausted pool = %+v", got)
	}
	if _, err := e.svc.ClaudePool(ctx, alice, other, "s1", ""); !errors.Is(err, hub.ErrForbidden) {
		t.Errorf("other backend for alice: %v", err)
	}
	if _, err := e.svc.ClaudePool(ctx, bob, other, "s1", ""); !errors.Is(err, hub.ErrNoClaude) {
		t.Errorf("backend without the plugin: %v", err)
	}
}

func TestAccountsCredits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.plugin.Lock()
	e.plugin.Credits[account] = &claude.Credits{Provider: "claude-api-account1", MonthlyUSD: pct(100), SpentUSD: pct(27.4),
		LeftUSD: pct(72.6), Since: time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), ResetsAt: time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC),
		UnpricedCalls: 2}
	e.plugin.Unlock()

	got, err := e.svc.ClaudeAccounts(ctx, bob, backendMain, true)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Accounts[0].Credits
	if c == nil || *c.LeftUSD != 72.6 || *c.MonthlyUSD != 100 || c.ResetsText != "Dec 1" || c.Unpriced != 2 || c.Provider != "claude-api-account1" {
		t.Fatalf("credits = %+v", c)
	}
	if got.Accounts[1].Credits != nil {
		t.Errorf("an account without a key has credits: %+v", got.Accounts[1].Credits)
	}
}
