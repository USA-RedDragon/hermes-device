package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/notify"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/webpush"
)

const (
	alice    = "alice"
	bob      = "bob"
	backend  = "desk"
	session  = "s1"
	endpoint = "https://fcm.googleapis.com/fcm/send/abc"
	command  = "rm -rf /secret-path"
	title    = "Secret project chat"
)

type access map[string]string

func (a access) Allowed(_ context.Context, user, backendID string) error {
	if a[user] == backendID {
		return nil
	}
	return hub.ErrForbidden
}

type sentTG struct {
	chat int64
	m    notify.Message
}

type fakeTG struct {
	mu      sync.Mutex
	sent    []sentTG
	settled [][]hub.NoticeMessage
	// cancelled are the cancelled flags of the settled calls.
	cancelled []bool
	outcomes  []hub.ApprovalOutcome
	next      int64
}

func (f *fakeTG) SendNotice(_ context.Context, chatID int64, m notify.Message) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.sent = append(f.sent, sentTG{chatID, m})
	return f.next, nil
}

func (f *fakeTG) Settled(_ context.Context, _ hub.NoticeRecord, cancelled bool, msgs []hub.NoticeMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, msgs)
	f.cancelled = append(f.cancelled, cancelled)
}

func (f *fakeTG) Outcome(_ context.Context, _ hub.NoticeRecord, o hub.ApprovalOutcome, _ []hub.NoticeMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, o)
}

func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type pushed struct {
	sub     webpush.Subscription
	payload notify.PushPayload
	raw     string
	urgency string
}

type fakePush struct {
	mu   sync.Mutex
	sent []pushed
	gone bool
}

func (f *fakePush) PublicKey() string { return "pub" }

func (f *fakePush) Send(_ context.Context, _ *http.Client, sub webpush.Subscription, _ string, m webpush.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return webpush.ErrGone
	}
	var p notify.PushPayload
	_ = json.Unmarshal(m.Payload, &p)
	f.sent = append(f.sent, pushed{sub, p, string(m.Payload), m.Urgency})
	return nil
}

func (f *fakePush) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type pending struct{ gone bool }

func (p *pending) PendingPrompt(context.Context, string, string, string, string) (hub.PromptInfo, error) {
	if p.gone {
		return hub.PromptInfo{}, hub.ErrUnknownApproval
	}
	return hub.PromptInfo{ID: "p1"}, nil
}

func (p *pending) AnswerPrompt(context.Context, string, string, string, string, hub.Answer) error {
	return nil
}

type env struct {
	st   *store.Store
	tg   *fakeTG
	push *fakePush
	n    *notify.Notifier
	now  time.Time
	ctx  context.Context //nolint:containedctx // the tests' one context
	pend *pending
	jobs *fakeJobs
}

func setup(t *testing.T, mutate func(*notify.Options)) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{st: st, tg: &fakeTG{}, push: &fakePush{}, now: time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC), ctx: context.Background(),
		pend: &pending{}, jobs: &fakeJobs{}}
	st.SetClock(func() time.Time { return e.now })
	o := notify.Options{Store: st, Access: access{alice: backend, bob: "other"}, Prompts: e.pend, Users: []string{alice, bob},
		Backends: map[string]string{backend: "Desk"}, Jobs: map[string]notify.Requester{backend: e.jobs},
		PublicURL: "https://hub.example/", Push: e.push, PushSubject: "https://hub.example",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }}
	if mutate != nil {
		mutate(&o)
	}
	e.n = notify.New(o)
	e.n.SetTelegram(e.tg)
	for _, u := range []struct {
		name string
		tg   int64
	}{{alice, 11}, {bob, 22}} {
		if err := st.LinkTelegram(e.ctx, u.tg, u.name); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SavePushSubscription(e.ctx, hub.PushSubscription{User: u.name, Endpoint: endpoint + u.name, P256dh: "k", Auth: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func approval(key string) hub.Notice {
	return hub.Notice{Kind: hub.NoticeApproval, BackendID: backend, SessionID: session, Key: key, Title: title,
		Prompt: &hub.PromptInfo{ID: "p1", Kind: hub.PromptApproval, Command: command, Choices: []string{"once", "deny"}}}
}

func TestApprovalReachesOnlyItsPersonOnce(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.n.HandleNow(e.ctx, approval("prompt:s1:p1"))
	e.n.HandleNow(e.ctx, approval("prompt:s1:p1"))
	if e.tg.count() != 1 || e.push.count() != 1 {
		t.Fatalf("telegram %d, push %d", e.tg.count(), e.push.count())
	}
	got := e.tg.sent[0]
	if got.chat != 11 || got.m.Record.User != alice || got.m.Title != title || got.m.URL != "https://hub.example/#/s/desk/s1" {
		t.Fatalf("telegram = %+v", got)
	}
	p := e.push.sent[0]
	if p.sub.Endpoint != endpoint+alice || p.payload.Kind != hub.NoticeApproval || p.payload.URL != "/#/s/desk/s1" || p.urgency != "high" {
		t.Fatalf("push = %+v", p)
	}
	if strings.Contains(p.raw, "secret") || strings.Contains(p.raw, "Secret") {
		t.Fatalf("push payload leaks chat content: %s", p.raw)
	}
}

func TestSettingsQuietHoursAndPresence(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	set := hub.DefaultNotifySettings()
	set.Approvals = false
	_ = e.st.SetNotifySettings(e.ctx, alice, set)
	e.n.HandleNow(e.ctx, approval("k1"))
	set = hub.DefaultNotifySettings()
	set.QuietStart, set.QuietEnd, set.TimeZone = "08:00", "10:00", "America/Chicago"
	_ = e.st.SetNotifySettings(e.ctx, alice, set)
	e.n.HandleNow(e.ctx, approval("k2"))
	_ = e.st.SetNotifySettings(e.ctx, alice, hub.DefaultNotifySettings())
	_ = e.st.SetPresence(e.ctx, alice, backend, session, true)
	e.n.HandleNow(e.ctx, approval("k3"))
	if e.tg.count() != 0 || e.push.count() != 0 {
		t.Fatalf("sent while off, quiet or looking: telegram %d, push %d", e.tg.count(), e.push.count())
	}
	_ = e.st.SaveTelegramChat(e.ctx, hub.TelegramChat{ChatID: 11, User: alice, BackendID: backend, SessionID: session})
	_ = e.st.SetPresence(e.ctx, alice, backend, session, false)
	e.n.HandleNow(e.ctx, approval("k4"))
	if e.tg.count() != 0 || e.push.count() != 1 {
		t.Fatalf("attached chat: telegram %d, push %d", e.tg.count(), e.push.count())
	}
}

func TestOneReminder(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	_ = e.st.SetPresence(e.ctx, alice, backend, session, true)
	e.n.HandleNow(e.ctx, approval("prompt:s1:p1"))
	e.now = e.now.Add(3 * time.Minute)
	e.n.RemindNow(e.ctx)
	if e.tg.count() != 0 {
		t.Fatal("reminded too early or while looking")
	}
	e.now = e.now.Add(4 * time.Minute)
	e.n.RemindNow(e.ctx)
	e.n.RemindNow(e.ctx)
	if e.tg.count() != 1 || e.push.count() != 1 || !e.tg.sent[0].m.Reminder {
		t.Fatalf("reminders: telegram %+v, push %d", e.tg.sent, e.push.count())
	}
	if !strings.HasPrefix(e.push.sent[0].payload.Title, "Still waiting") {
		t.Fatalf("reminder title = %q", e.push.sent[0].payload.Title)
	}
}

func TestSettledPromptGetsNoReminder(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.n.HandleNow(e.ctx, approval("prompt:s1:p1"))
	e.n.HandleNow(e.ctx, hub.Notice{Kind: hub.NoticeSettled, BackendID: backend, SessionID: session, PromptIDs: []string{"p1"}})
	if len(e.tg.settled) != 1 || e.tg.settled[0][0].ChatID != 11 {
		t.Fatalf("settled = %+v", e.tg.settled)
	}
	e.now = e.now.Add(time.Hour)
	e.n.RemindNow(e.ctx)
	if e.tg.count() != 1 {
		t.Fatal("reminded a settled prompt")
	}
	e.n.HandleNow(e.ctx, approval("prompt:s1:p2"))
	e.pend.gone = true
	e.now = e.now.Add(time.Hour)
	e.n.RemindNow(e.ctx)
	if e.tg.count() != 2 {
		t.Fatalf("reminded a prompt Hermes no longer has: %d", e.tg.count())
	}
}

func TestOutcomeShowsOnceOnSettledNotices(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	ask := func(id string) {
		n := approval("prompt:s1:" + id)
		n.Prompt.ID = id
		e.n.HandleNow(e.ctx, n)
	}
	settle := func(id string) {
		e.n.HandleNow(e.ctx, hub.Notice{Kind: hub.NoticeSettled, BackendID: backend, SessionID: session, PromptIDs: []string{id}})
	}
	outcome := func(id string) {
		e.n.HandleNow(e.ctx, hub.Notice{Kind: hub.NoticeOutcome, BackendID: backend, SessionID: session, PromptIDs: []string{id},
			Outcome: &hub.ApprovalOutcome{PromptID: id, Allowed: true, Via: hub.AnsweredElsewhere}})
	}
	ask("p1")
	settle("p1")
	outcome("p2")
	if len(e.tg.outcomes) != 0 {
		t.Fatalf("outcome of a prompt with no notice shown: %+v", e.tg.outcomes)
	}
	outcome("p1")
	outcome("p1")
	if len(e.tg.outcomes) != 1 || e.tg.outcomes[0].PromptID != "p1" {
		t.Fatalf("outcomes = %+v", e.tg.outcomes)
	}
	if _, err := e.st.RecordAnswer(e.ctx, backend, "p3", hub.PromptAnswer{Choice: hub.ChoiceOnce, Via: hub.AnsweredInHub}); err != nil {
		t.Fatal(err)
	}
	ask("p3")
	settle("p3")
	outcome("p3")
	if len(e.tg.outcomes) != 1 {
		t.Fatalf("outcome shown over a recorded answer: %+v", e.tg.outcomes)
	}
}

func TestLinkedGroupGetsNoticesInGeneral(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	const group = -1001234
	if err := e.st.LinkTelegramGroup(e.ctx, hub.TelegramGroup{User: alice, ChatID: group, LinkedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	e.n.HandleNow(e.ctx, approval("k1"))
	if e.tg.count() != 1 || e.tg.sent[0].chat != group {
		t.Fatalf("sent = %+v", e.tg.sent)
	}
	if err := e.st.SaveTelegramTopic(e.ctx, hub.TelegramTopic{ChatID: group, ThreadID: 7, User: alice, BackendID: backend,
		SessionID: session, Name: title, Seen: e.now}); err != nil {
		t.Fatal(err)
	}
	e.n.HandleNow(e.ctx, approval("k2"))
	if e.tg.count() != 1 {
		t.Fatalf("a prompt of a chat with a topic got a notice in General too: %+v", e.tg.sent)
	}
	e.n.HandleNow(e.ctx, hub.Notice{Kind: hub.NoticeFailure, BackendID: backend, SessionID: session, Key: "f1", Title: title,
		Detail: "needs a sign-in"})
	if e.tg.count() != 2 || e.tg.sent[1].chat != group || e.tg.sent[1].m.Record.Kind != hub.NoticeFailure {
		t.Fatalf("a failure of a chat with a topic did not reach General: %+v", e.tg.sent)
	}
	e.n.HandleNow(e.ctx, hub.Notice{Kind: hub.NoticeSettled, BackendID: backend, SessionID: session, PromptIDs: []string{"p1"},
		Cancelled: true})
	if len(e.tg.cancelled) != 1 || !e.tg.cancelled[0] {
		t.Fatalf("settled = %+v, cancelled = %v", e.tg.settled, e.tg.cancelled)
	}
}

func TestBurstLimit(t *testing.T) {
	t.Parallel()
	e := setup(t, func(o *notify.Options) { o.Burst = 2 })
	for _, k := range []string{"a", "b", "c", "d"} {
		e.n.HandleNow(e.ctx, approval(k))
	}
	if e.tg.count() != 2 {
		t.Fatalf("sent %d, want 2", e.tg.count())
	}
}

func TestGoneSubscriptionIsDeleted(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.push.gone = true
	e.n.HandleNow(e.ctx, approval("k"))
	if subs, _ := e.st.PushSubscriptions(e.ctx, alice); len(subs) != 0 {
		t.Fatalf("subscriptions = %+v", subs)
	}
	if subs, _ := e.st.PushSubscriptions(e.ctx, bob); len(subs) != 1 {
		t.Fatalf("bob's subscription went too: %+v", subs)
	}
}

type fakeJobs struct {
	mu   sync.Mutex
	body string
}

func (f *fakeJobs) Request(_ context.Context, method, path string, _ []byte) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method != http.MethodGet || path != "/api/cron/jobs" {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func (f *fakeJobs) set(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func TestScheduledJobs(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	_ = e.st.SetNotifySettings(e.ctx, alice, hub.NotifySettings{Jobs: true, Failures: true, Telegram: true, Push: true})
	e.jobs.set(`[{"id":"j1","name":"Nightly","last_run_at":"2026-11-10T03:00:00","last_status":"ok"}]`)
	e.n.PollJobsNow(e.ctx)
	if e.tg.count() != 0 {
		t.Fatal("first sight of a job sent a notice")
	}
	e.jobs.set(`[{"id":"j1","name":"Nightly","last_run_at":"2026-11-11T03:00:00","last_status":"ok"}]`)
	e.n.PollJobsNow(e.ctx)
	e.n.PollJobsNow(e.ctx)
	if e.tg.count() != 1 || e.tg.sent[0].m.Record.Kind != hub.NoticeJob || !strings.Contains(e.tg.sent[0].m.Detail, "Nightly finished") {
		t.Fatalf("job notices = %+v", e.tg.sent)
	}
	if e.tg.sent[0].m.URL != "https://hub.example/" {
		t.Fatalf("job url = %q", e.tg.sent[0].m.URL)
	}
	e.jobs.set(`[{"id":"j1","name":"Nightly","last_run_at":"2026-11-12T03:00:00","last_status":"error","last_error":"HTTP 401: please log in again, token sk-abcdefghijklmnopqrstuvwxyz0123"}]`)
	e.n.PollJobsNow(e.ctx)
	if e.tg.count() != 2 || e.tg.sent[1].m.Record.Kind != hub.NoticeFailure || !strings.Contains(e.tg.sent[1].m.Detail, "needs you to sign in") {
		t.Fatalf("failure notices = %+v", e.tg.sent)
	}
	if strings.Contains(e.tg.sent[1].m.Detail, "sk-abc") {
		t.Fatalf("job notice shows a token: %q", e.tg.sent[1].m.Detail)
	}
	if strings.Contains(e.push.sent[1].raw, "401") {
		t.Fatalf("push leaks the error: %s", e.push.sent[1].raw)
	}
}

func TestQuietHours(t *testing.T) {
	t.Parallel()
	s := hub.NotifySettings{QuietStart: "22:00", QuietEnd: "07:00", TimeZone: "America/Chicago"}
	for _, c := range []struct {
		utc  string
		want bool
	}{{"2026-11-11T04:30:00Z", true}, {"2026-11-11T12:30:00Z", true}, {"2026-11-11T13:00:00Z", false}, {"2026-11-11T18:00:00Z", false}} {
		at, _ := time.Parse(time.RFC3339, c.utc)
		if got := notify.Quiet(s, at); got != c.want {
			t.Errorf("%s: quiet = %v", c.utc, got)
		}
	}
	if notify.Quiet(hub.NotifySettings{}, time.Now()) {
		t.Error("no quiet hours set, but quiet")
	}
	for _, bad := range []hub.NotifySettings{{QuietStart: "22:00"}, {QuietStart: "25:00", QuietEnd: "07:00"}, {TimeZone: "Mars/Base"}} {
		if notify.ValidSettings(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := notify.ValidSettings(s); err != nil {
		t.Error(err)
	}
}

func TestHeldBackNoticesGetNoReminder(t *testing.T) {
	t.Parallel()
	e := setup(t, func(o *notify.Options) { o.Burst = 2 })
	for _, k := range []string{"a", "b", "c", "d"} {
		e.n.HandleNow(e.ctx, approval(k))
	}
	e.now = e.now.Add(11 * time.Minute)
	e.n.RemindNow(e.ctx)
	e.n.RemindNow(e.ctx)
	if e.tg.count() != 4 {
		t.Fatalf("sent %d, want 2 notices and 2 reminders", e.tg.count())
	}
}

func TestQuietPersonDoesNotBlockReminders(t *testing.T) {
	t.Parallel()
	e := setup(t, func(o *notify.Options) {
		o.Burst = 1000
		o.Access = access{alice: backend, bob: backend}
	})
	set := hub.DefaultNotifySettings()
	set.QuietStart, set.QuietEnd = "00:00", "23:59"
	_ = e.st.SetNotifySettings(e.ctx, alice, set)
	for i := range 101 {
		e.n.HandleNow(e.ctx, approval("k"+strconv.Itoa(i)))
	}
	sent := e.tg.count()
	e.now = e.now.Add(6 * time.Minute)
	for range 3 {
		e.n.RemindNow(e.ctx)
	}
	if got := e.tg.count() - sent; got != 101 {
		t.Fatalf("bob got %d reminders, want 101", got)
	}
}

func TestClaudeSignInGoesOnlyToItsUsers(t *testing.T) {
	t.Parallel()
	e := setup(t, func(o *notify.Options) { o.Access = access{alice: backend, bob: backend} })
	_ = e.st.SaveTelegramChat(e.ctx, hub.TelegramChat{ChatID: 11, User: alice, BackendID: backend, SessionID: session})
	detail := "Claude (account1) (last signed in as alice@example.com) needs a sign-in: the login expired or was revoked. Open the hub to sign in."
	n := hub.Notice{Kind: hub.NoticeClaudeSignIn, BackendID: backend, SessionID: session, Key: "claude:desk:account1:1",
		Title: title, Detail: detail, Users: []string{alice}}
	e.n.HandleNow(e.ctx, n)
	e.n.HandleNow(e.ctx, n)
	if e.tg.count() != 1 || e.push.count() != 1 {
		t.Fatalf("telegram %d, push %d", e.tg.count(), e.push.count())
	}
	if got := e.tg.sent[0]; got.chat != 11 || got.m.Detail != detail || got.m.URL != "https://hub.example/#/s/desk/s1" {
		t.Fatalf("telegram = %+v", got)
	}
	p := e.push.sent[0].payload
	if p.Title != "A Claude account needs a sign-in" || p.Body != "On Desk. "+detail || e.push.sent[0].sub.Endpoint != endpoint+alice {
		t.Fatalf("push = %+v", p)
	}
	set := hub.DefaultNotifySettings()
	set.ClaudeSignIn = false
	_ = e.st.SetNotifySettings(e.ctx, alice, set)
	n.Key = "claude:desk:account1:2"
	e.n.HandleNow(e.ctx, n)
	if e.tg.count() != 1 || e.push.count() != 1 {
		t.Fatalf("sent with the setting off: telegram %d, push %d", e.tg.count(), e.push.count())
	}
}
