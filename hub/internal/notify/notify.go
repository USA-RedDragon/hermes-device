// Package notify tells people about what their Hermes does while they are
// away: approvals and questions that wait for them, long turns that ended,
// scheduled job results and failures. It sends each notice once, to the
// person's linked Telegram chat and their browsers' Web Push
// subscriptions, as their settings allow, and reminds them once of a
// prompt that still waits.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // quiet hours need time zones; the image has no tzdata

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/webpush"
)

// Telegram sends notices to Telegram chats.
type Telegram interface {
	// SendNotice sends a notice to a private chat, or to the General
	// topic of a linked group, and returns the message's ID.
	SendNotice(ctx context.Context, chatID int64, n Message) (int64, error)
	// Settled marks the messages of a notice whose prompt got its answer,
	// or was cancelled.
	Settled(ctx context.Context, rec hub.NoticeRecord, cancelled bool, msgs []hub.NoticeMessage)
}

// Message is a notice for one person, as a front end shows it.
type Message struct {
	Record   hub.NoticeRecord
	Reminder bool
	// Title is the chat's title, "" when unknown.
	Title string
	// Backend is the backend's display name.
	Backend string
	Detail  string
	// URL opens the chat, or the hub, with the person's normal login.
	URL string
}

// Access says whether a person may use a backend.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Requester sends signed-in requests to a backend's Hermes web server.
type Requester interface {
	Request(ctx context.Context, method, path string, body []byte) (*http.Response, error)
}

// Pusher sends Web Push messages; *webpush.VAPID is one.
type Pusher interface {
	PublicKey() string
	Send(ctx context.Context, client *http.Client, sub webpush.Subscription, subject string, m webpush.Message) error
}

// Options are a Notifier's dependencies and limits. Zero limits pick the
// defaults.
type Options struct {
	Store   hub.NotifyStore
	Access  Access
	Prompts hub.Prompter
	// Users are every hub user.
	Users []string
	// Backends maps each backend ID to its display name.
	Backends map[string]string
	// Jobs reads each backend's scheduled jobs; a backend without one is
	// not polled.
	Jobs      map[string]Requester
	PublicURL string
	// Push sends Web Push messages; nil turns Web Push off.
	Push        Pusher
	PushSubject string
	HTTPClient  *http.Client
	Logger      *slog.Logger

	// Remind is how long a prompt waits before its one reminder. Default 5m.
	Remind time.Duration
	// Presence is how recent a page's visit must be to count as looking
	// at the chat. Default 90s.
	Presence time.Duration
	// Burst and BurstWindow cap how many notices a person gets: at most
	// Burst in each BurstWindow. Defaults 20 and 10m.
	Burst       int
	BurstWindow time.Duration
	// JobPoll is how often the leader reads scheduled jobs. Default 1m.
	JobPoll time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Notifier delivers notices.
type Notifier struct {
	o   Options
	log *slog.Logger
	in  chan hub.Notice
	// settled is apart from in, so a flood of new notices cannot crowd
	// out the news that a prompt got its answer.
	settled chan hub.Notice
	tg      Telegram
}

// New returns a Notifier. SetTelegram adds the Telegram channel.
func New(o Options) *Notifier {
	if o.Remind == 0 {
		o.Remind = 5 * time.Minute
	}
	if o.Presence == 0 {
		o.Presence = 90 * time.Second
	}
	if o.Burst == 0 {
		o.Burst = 20
	}
	if o.BurstWindow == 0 {
		o.BurstWindow = 10 * time.Minute
	}
	if o.JobPoll == 0 {
		o.JobPoll = time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	return &Notifier{o: o, log: o.Logger.With("component", "notify"), in: make(chan hub.Notice, 1024),
		settled: make(chan hub.Notice, 1024)}
}

// SetTelegram sets the Telegram channel. Call it before Run.
func (n *Notifier) SetTelegram(t Telegram) { n.tg = t }

// PushKey is the VAPID public key browsers subscribe with, "" when Web
// Push is off.
func (n *Notifier) PushKey() string {
	if n.o.Push == nil {
		return ""
	}
	return n.o.Push.PublicKey()
}

// Notify queues a notice. It never blocks: when the queue is full the
// notice is dropped.
func (n *Notifier) Notify(nt hub.Notice) {
	ch := n.in
	if nt.Kind == hub.NoticeSettled {
		ch = n.settled
	}
	select {
	case ch <- nt:
	default:
		n.log.Warn("Notice queue full, dropping a notice", "kind", nt.Kind)
	}
}

// Run delivers queued notices until ctx ends. Run it on every replica:
// notices come from the sessions each replica follows, and the store
// keeps the same notice from going out twice.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case nt := <-n.settled:
			n.handle(ctx, nt)
		case nt := <-n.in:
			n.handle(ctx, nt)
		}
	}
}

// Lead sends reminders and reads scheduled jobs until ctx ends. Run it
// on one replica.
func (n *Notifier) Lead(ctx context.Context) {
	remind := time.NewTicker(30 * time.Second)
	defer remind.Stop()
	jobs := time.NewTicker(n.o.JobPoll)
	defer jobs.Stop()
	n.pollJobs(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-remind.C:
			n.remind(ctx)
			n.prune(ctx)
		case <-jobs.C:
			n.pollJobs(ctx)
		}
	}
}

func (n *Notifier) handle(ctx context.Context, nt hub.Notice) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if nt.Kind == hub.NoticeSettled {
		n.settle(cctx, nt.BackendID, nt.SessionID, nt.PromptIDs, nt.Cancelled)
		return
	}
	users := n.o.Users
	if nt.Users != nil {
		users = nt.Users
	}
	for _, user := range users {
		if n.o.Access.Allowed(cctx, user, nt.BackendID) != nil {
			continue
		}
		n.deliverNew(cctx, user, nt)
	}
}

func (n *Notifier) settle(ctx context.Context, backendID, sessionID string, ids []string, cancelled bool) {
	recs, err := n.o.Store.SettleNotices(ctx, backendID, sessionID, ids)
	if err != nil {
		n.log.Warn("Failed to settle notices", "error", err.Error())
		return
	}
	if n.tg == nil {
		return
	}
	for _, r := range recs {
		msgs, err := n.o.Store.NoticeMessages(ctx, r.ID)
		if err == nil && len(msgs) > 0 {
			n.tg.Settled(ctx, r, cancelled, msgs)
		}
	}
}

func (n *Notifier) deliverNew(ctx context.Context, user string, nt hub.Notice) {
	set, err := n.o.Store.NotifySettings(ctx, user)
	if err != nil {
		n.log.Warn("Failed to read notification settings", "user", user, "error", err.Error())
		return
	}
	if !set.Wants(nt.Kind) {
		return
	}
	rec := hub.NoticeRecord{User: user, Key: nt.Key, Kind: nt.Kind, BackendID: nt.BackendID, SessionID: nt.SessionID, Prompt: nt.Prompt,
		Account: nt.Account}
	if nt.Prompt != nil {
		rec.PromptID = nt.Prompt.ID
	}
	id, claimed, err := n.o.Store.ClaimNotice(ctx, rec)
	if err != nil || !claimed {
		if err != nil {
			n.log.Warn("Failed to store a notice", "user", user, "error", err.Error())
		}
		return
	}
	rec.ID, rec.CreatedAt = id, n.o.Now()
	if quiet(set, n.o.Now()) || n.present(ctx, user, nt.BackendID, nt.SessionID) {
		return
	}
	if n.flooded(ctx, user) {
		n.log.Warn("Too many notices, holding back", "user", user, "kind", nt.Kind)
		_, _ = n.o.Store.ClaimReminder(ctx, id)
		return
	}
	n.deliver(ctx, set, Message{Record: rec, Title: nt.Title, Detail: nt.Detail})
}

// flooded reports whether a person had more notices than the burst
// limit allows, or whether that cannot be read.
func (n *Notifier) flooded(ctx context.Context, user string) bool {
	sent, err := n.o.Store.SentSince(ctx, user, n.o.Now().Add(-n.o.BurstWindow))
	return err != nil || sent > n.o.Burst
}

func (n *Notifier) present(ctx context.Context, user, backendID, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	ok, err := n.o.Store.Present(ctx, user, backendID, sessionID, n.o.Now().Add(-n.o.Presence))
	return err == nil && ok
}

// chatURL opens the chat of a notice in the hub, or the hub itself.
func (n *Notifier) chatURL(r hub.NoticeRecord) string {
	if r.SessionID == "" {
		return n.o.PublicURL + "/"
	}
	return n.o.PublicURL + "/#/s/" + url.PathEscape(r.BackendID) + "/" + url.PathEscape(r.SessionID)
}

func (n *Notifier) deliver(ctx context.Context, set hub.NotifySettings, m Message) {
	r := m.Record
	m.Backend = n.o.Backends[r.BackendID]
	if m.Backend == "" {
		m.Backend = r.BackendID
	}
	m.URL = n.chatURL(r)
	if set.Telegram && n.tg != nil {
		n.sendTelegram(ctx, m)
	}
	if set.Push && n.o.Push != nil {
		n.sendPush(ctx, m)
	}
}

func (n *Notifier) sendTelegram(ctx context.Context, m Message) {
	r := m.Record
	ids, grouped, err := n.telegramChats(ctx, r.User)
	if err != nil {
		n.log.Warn("Failed to look up Telegram links", "user", r.User, "error", err.Error())
		return
	}
	// A chat that Telegram shows already shows its prompts and results. In
	// a linked group its topic shows the prompts, and General gets the
	// rest with a button to the topic.
	prompt := r.Kind == hub.NoticeApproval || r.Kind == hub.NoticeQuestion
	if !m.Reminder && r.SessionID != "" && r.Kind != hub.NoticeClaudeSignIn && (prompt || !grouped) {
		if attached, err := n.o.Store.TelegramAttached(ctx, r.User, r.BackendID, r.SessionID); err != nil || attached {
			return
		}
	}
	for _, chat := range ids {
		msgID, err := n.tg.SendNotice(ctx, chat, m)
		if err != nil {
			n.log.Warn("Failed to send a Telegram notice", "user", r.User, "error", err.Error())
			continue
		}
		if err := n.o.Store.AddNoticeMessage(ctx, r.ID, hub.NoticeMessage{ChatID: chat, MessageID: msgID}); err != nil {
			n.log.Warn("Failed to record a Telegram notice", "error", err.Error())
		}
	}
}

// telegramChats are where a person's notices go on Telegram: the General
// topic of their linked group, or else each private chat linked to them.
// grouped is true for a group.
func (n *Notifier) telegramChats(ctx context.Context, user string) (ids []int64, grouped bool, err error) {
	group, ok, err := n.o.Store.TelegramGroup(ctx, user)
	if err != nil {
		return nil, false, err
	}
	if ok {
		return []int64{group}, true, nil
	}
	ids, err = n.o.Store.TelegramIDs(ctx, user)
	return ids, false, err
}

// PushPayload is the whole of a Web Push message: the event kind and a
// short title of the hub's own words, never chat content. A Claude
// sign-in notice also names the account in its body. The click
// opens URL, which loads the hub with the person's normal login.
type PushPayload struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

// Titles are the fixed titles of each notice kind.
func Titles(kind string) string {
	switch kind {
	case hub.NoticeApproval:
		return "Hermes needs your approval"
	case hub.NoticeQuestion:
		return "Hermes has a question"
	case hub.NoticeFinished:
		return "Hermes finished a long task"
	case hub.NoticeJob:
		return "A scheduled job finished"
	case hub.NoticeFailure:
		return "Something failed in Hermes"
	case hub.NoticeClaudeSignIn:
		return "A Claude account needs a sign-in"
	case KindTest:
		return "Test notification from Hermes Hub"
	}
	return "Hermes"
}

func pushPayload(m Message) PushPayload {
	title := Titles(m.Record.Kind)
	if m.Reminder {
		title = "Still waiting: " + strings.ToLower(title[:1]) + title[1:]
	}
	body := "On " + m.Backend + ". Open the hub to see it."
	if m.Record.Kind == hub.NoticeClaudeSignIn && m.Detail != "" {
		body = "On " + m.Backend + ". " + m.Detail
	}
	return PushPayload{Kind: m.Record.Kind, Title: title, Body: body, URL: m.URL, Tag: "hub-" + m.Record.ID}
}

func (n *Notifier) sendPush(ctx context.Context, m Message) {
	subs, err := n.o.Store.PushSubscriptions(ctx, m.Record.User)
	if err != nil || len(subs) == 0 {
		return
	}
	p := pushPayload(m)
	if u, err := url.Parse(p.URL); err == nil {
		p.URL = u.RequestURI()
		if u.Fragment != "" {
			p.URL += "#" + u.Fragment
		}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	urgency := "normal"
	if m.Record.Kind == hub.NoticeApproval || m.Record.Kind == hub.NoticeQuestion {
		urgency = "high"
	}
	for _, s := range subs {
		err := n.o.Push.Send(ctx, n.o.HTTPClient, webpush.Subscription{Endpoint: s.Endpoint, P256dh: s.P256dh, Auth: s.Auth},
			n.o.PushSubject, webpush.Message{Payload: body, TTL: 12 * time.Hour, Urgency: urgency})
		switch {
		case errors.Is(err, webpush.ErrGone):
			if _, err := n.o.Store.DeletePushSubscription(ctx, s.User, s.ID); err != nil {
				n.log.Warn("Failed to delete a gone push subscription", "error", err.Error())
			}
		case err != nil:
			n.log.Warn("Failed to send a push", "user", s.User, "error", pushError(err))
		}
	}
}

// KindTest is the kind of a test notice a person sends themselves.
const KindTest = "test"

// TestResult says where a test notice went.
type TestResult struct {
	Telegram int `json:"telegram"`
	Push     int `json:"push"`
}

// Test sends a person a test notice on each channel their settings turn
// on, at most once a minute.
func (n *Notifier) Test(ctx context.Context, user string) (TestResult, error) {
	var res TestResult
	set, err := n.o.Store.NotifySettings(ctx, user)
	if err != nil {
		return res, err
	}
	rec := hub.NoticeRecord{User: user, Kind: KindTest, Key: "test:" + strconv.FormatInt(n.o.Now().Unix()/60, 10)}
	id, claimed, err := n.o.Store.ClaimNotice(ctx, rec)
	if err != nil {
		return res, err
	}
	if !claimed {
		return res, ErrTooSoon
	}
	rec.ID = id
	m := Message{Record: rec, Backend: "Hermes Hub", Detail: "This is a test. Notices about your chats look like this.", URL: n.o.PublicURL + "/"}
	if set.Telegram && n.tg != nil {
		ids, _, err := n.telegramChats(ctx, user)
		if err != nil {
			return res, err
		}
		for _, chat := range ids {
			if _, err := n.tg.SendNotice(ctx, chat, m); err == nil {
				res.Telegram++
			}
		}
	}
	if set.Push && n.o.Push != nil {
		subs, err := n.o.Store.PushSubscriptions(ctx, user)
		if err != nil {
			return res, err
		}
		n.sendPush(ctx, m)
		res.Push = len(subs)
	}
	return res, nil
}

// ErrTooSoon is a second test notice within a minute.
var ErrTooSoon = errors.New("wait a minute before the next test")

// pushError describes a failed push without the subscription's endpoint,
// which works like a password for it.
func pushError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

// maxReminderAge is how old a prompt may be and still get its reminder.
const maxReminderAge = 12 * time.Hour

func (n *Notifier) remind(ctx context.Context) {
	now := n.o.Now()
	due, err := n.o.Store.DueReminders(ctx, now.Add(-n.o.Remind), now)
	if err != nil {
		n.log.Warn("Failed to list reminders", "error", err.Error())
		return
	}
	for _, r := range due {
		if ctx.Err() != nil {
			return
		}
		n.remindOne(ctx, now, r)
	}
}

func (n *Notifier) remindOne(ctx context.Context, now time.Time, r hub.NoticeRecord) {
	drop := func() { _, _ = n.o.Store.ClaimReminder(ctx, r.ID) }
	later := func(d time.Duration) { _ = n.o.Store.DeferReminder(ctx, r.ID, now.Add(d)) }
	if now.Sub(r.CreatedAt) > maxReminderAge {
		drop()
		return
	}
	set, err := n.o.Store.NotifySettings(ctx, r.User)
	switch {
	case err != nil:
		later(time.Minute)
		return
	case !set.Wants(r.Kind):
		drop()
		return
	case quiet(set, now):
		later(5 * time.Minute)
		return
	case n.present(ctx, r.User, r.BackendID, r.SessionID):
		later(time.Minute)
		return
	case n.flooded(ctx, r.User):
		later(n.o.BurstWindow)
		return
	}
	if n.o.Prompts != nil {
		_, err := n.o.Prompts.PendingPrompt(ctx, r.User, r.BackendID, r.SessionID, r.PromptID)
		switch {
		case errors.Is(err, hub.ErrUnknownApproval), errors.Is(err, hub.ErrUnknownSession):
			n.settle(ctx, r.BackendID, r.SessionID, []string{r.PromptID}, false)
			return
		case errors.Is(err, hub.ErrForbidden), errors.Is(err, hub.ErrUnknownBackend):
			drop()
			return
		case err != nil:
			later(time.Minute)
			return
		}
	}
	if ok, err := n.o.Store.ClaimReminder(ctx, r.ID); err != nil || !ok {
		return
	}
	n.deliver(ctx, set, Message{Record: r, Reminder: true})
}

// noticeTTL is how long the hub keeps notices and presence.
const noticeTTL = 72 * time.Hour

func (n *Notifier) prune(ctx context.Context) {
	if err := n.o.Store.PruneNotices(ctx, n.o.Now().Add(-noticeTTL)); err != nil {
		n.log.Warn("Failed to prune notices", "error", err.Error())
	}
}

// quiet reports whether t is in the settings' quiet hours.
func quiet(s hub.NotifySettings, t time.Time) bool {
	start, ok1 := clock(s.QuietStart)
	end, ok2 := clock(s.QuietEnd)
	if !ok1 || !ok2 || start == end {
		return false
	}
	if loc, err := time.LoadLocation(s.TimeZone); err == nil {
		t = t.In(loc)
	}
	m := t.Hour()*60 + t.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// clock reads "HH:MM" as minutes after midnight.
func clock(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// ValidSettings checks settings a person sends.
func ValidSettings(s hub.NotifySettings) error {
	if (s.QuietStart == "") != (s.QuietEnd == "") {
		return errors.New("set both ends of the quiet hours, or neither")
	}
	if s.QuietStart != "" {
		if _, ok := clock(s.QuietStart); !ok {
			return fmt.Errorf("quiet hours start %q is not HH:MM", s.QuietStart)
		}
		if _, ok := clock(s.QuietEnd); !ok {
			return fmt.Errorf("quiet hours end %q is not HH:MM", s.QuietEnd)
		}
	}
	if len(s.TimeZone) > 64 {
		return errors.New("unknown time zone")
	}
	if _, err := time.LoadLocation(s.TimeZone); err != nil {
		return errors.New("unknown time zone")
	}
	return nil
}
