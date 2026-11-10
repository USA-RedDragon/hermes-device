// Package telegram is the hub's Telegram front end. It long-polls the Bot
// API, links each Telegram account to a hub user, and relays that user's
// Hermes sessions into the chat.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	callTimeout  = 30 * time.Second
	maxBackoff   = 30 * time.Second
	maxListed    = 10
	pollSlack    = 15 * time.Second
	chatPrivate  = "private"
	chatGroup    = "group"
	chatSuper    = "supergroup"
	errGeneric   = "Something went wrong. Try again later."
	linkPath     = "/link/telegram"
	cbApproval   = "ap"
	cbAttach     = "at"
	untitledName = "(untitled)"
)

var tagRe = regexp.MustCompile(`<[^>]*>`)

// ErrMissingPublicURL is returned by New without Options.PublicURL.
var ErrMissingPublicURL = errors.New("telegram bot needs the hub public URL")

// Options holds the Bot's dependencies.
type Options struct {
	// PublicURL is the hub's public base URL (http.public_url); link
	// messages point at PublicURL + /link/telegram.
	PublicURL  string
	Service    hub.Service
	Links      hub.Links
	Tokens     hub.TokenStore
	Logger     *slog.Logger
	HTTPClient *http.Client
}

// Bot is the Telegram bot.
type Bot struct {
	cfg      Config
	link     string
	api      *Client
	svc      hub.Service
	links    hub.Links
	tokens   hub.TokenStore
	logger   *slog.Logger
	defaults map[string]string
	groups   map[int64]bool

	chats map[int64]*chatState
	wg    sync.WaitGroup

	mu        sync.Mutex
	approvals map[string]*pendingApproval
	nextKey   uint64
}

type chatState struct {
	user    string
	att     *attachment
	listGen int
	list    []hub.Session
}

type attachment struct {
	ctx       context.Context //nolint:containedctx // lives as long as the attachment; cancelled by stop
	cancel    context.CancelFunc
	chatID    int64
	user      string
	backendID string
	sessionID string
	events    <-chan hub.Event
	detach    func()
	done      chan struct{}
	lastSeq   atomic.Int64
	once      sync.Once
}

func (a *attachment) stop() {
	a.once.Do(func() {
		a.cancel()
		a.detach()
	})
	<-a.done
}

func (a *attachment) closed() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

type pendingApproval struct {
	chatID     int64
	messageID  int64
	backendID  string
	sessionID  string
	approvalID string
	text       string
}

// New validates cfg and returns a Bot.
func New(cfg Config, opts Options) (*Bot, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.PublicURL == "" {
		return nil, ErrMissingPublicURL
	}
	defaults, err := cfg.defaultBackends()
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	b := &Bot{
		cfg:       cfg,
		link:      strings.TrimRight(opts.PublicURL, "/") + linkPath,
		api:       NewClient(cfg.APIURL, cfg.Token, opts.HTTPClient, cfg.MaxRetries, logger),
		svc:       opts.Service,
		links:     opts.Links,
		tokens:    opts.Tokens,
		logger:    logger,
		defaults:  defaults,
		groups:    make(map[int64]bool, len(cfg.AllowedGroups)),
		chats:     make(map[int64]*chatState),
		approvals: make(map[string]*pendingApproval),
	}
	for _, id := range cfg.AllowedGroups {
		b.groups[id] = true
	}
	return b, nil
}

// Run long-polls Telegram until ctx ends, then detaches every session.
// Only one process may poll a bot token at a time.
func (b *Bot) Run(ctx context.Context) error {
	defer b.shutdown()
	var offset int64
	backoff := time.Second
	b.logger.Info("Telegram bot polling")
	for ctx.Err() == nil {
		pctx, cancel := context.WithTimeout(ctx, b.cfg.PollTimeout+pollSlack)
		updates, err := b.api.GetUpdates(pctx, offset, b.cfg.PollTimeout)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			b.logger.Error("Failed to get Telegram updates", "error", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				break
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			offset = u.UpdateID + 1
			b.handleUpdate(ctx, u)
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (b *Bot) shutdown() {
	for _, st := range b.chats {
		if st.att != nil {
			st.att.stop()
		}
	}
	b.wg.Wait()
}

func (b *Bot) handleUpdate(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		b.handleMessage(ctx, u.Message)
	case u.CallbackQuery != nil:
		b.handleCallback(ctx, u.CallbackQuery)
	}
}

func (b *Bot) chatAllowed(c Chat) bool {
	switch c.Type {
	case chatPrivate:
		return true
	case chatGroup, chatSuper:
		return b.groups[c.ID]
	default:
		return false
	}
}

func (b *Bot) chat(id int64) *chatState {
	st, ok := b.chats[id]
	if !ok {
		st = &chatState{}
		b.chats[id] = st
	}
	return st
}

func (b *Bot) lookupUser(ctx context.Context, telegramID int64) (string, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	user, ok, err := b.links.UserForTelegram(cctx, telegramID)
	if err != nil {
		b.logger.Error("Failed to look up Telegram link", "telegram_id", telegramID, "error", err)
		return "", false, fmt.Errorf("failed to look up Telegram link: %w", err)
	}
	return user, ok && user != "", nil
}

func (b *Bot) handleMessage(ctx context.Context, m *Message) {
	if m.From == nil || m.From.IsBot || m.Text == "" || !b.chatAllowed(m.Chat) {
		return
	}
	user, linked, err := b.lookupUser(ctx, m.From.ID)
	if err != nil {
		b.sendText(ctx, m.Chat.ID, errGeneric)
		return
	}
	if !linked {
		b.sendLink(ctx, m)
		return
	}
	cmd, arg := parseCommand(m.Text)
	b.logger.Debug("Telegram message", "user", user, "chat", m.Chat.ID, "command", cmd)
	switch cmd {
	case "":
		b.prompt(ctx, m.Chat.ID, user, m.Text)
	case "start", "help":
		b.help(ctx, m.Chat.ID, user)
	case "backends":
		b.listBackends(ctx, m.Chat.ID, user)
	case "sessions":
		b.listSessions(ctx, m.Chat.ID, user)
	case "attach":
		b.attachCommand(ctx, m.Chat.ID, user, arg)
	case "new":
		b.newSession(ctx, m.Chat.ID, user, arg)
	case "stop":
		b.stop(ctx, m.Chat.ID, user)
	default:
		b.sendText(ctx, m.Chat.ID, "Unknown command. Send /help for the list.")
	}
}

func parseCommand(text string) (string, string) {
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	head, arg, _ := strings.Cut(strings.TrimSpace(text[1:]), " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.ToLower(head), strings.TrimSpace(arg)
}

func (b *Bot) sendLink(ctx context.Context, m *Message) {
	if m.Chat.Type != chatPrivate {
		b.sendText(ctx, m.Chat.ID, "Send me a direct message to link your Telegram account first.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	token, err := b.tokens.CreateLinkToken(cctx, m.From.ID)
	if err != nil {
		b.logger.Error("Failed to create link token", "telegram_id", m.From.ID, "error", err)
		b.sendText(ctx, m.Chat.ID, errGeneric)
		return
	}
	link := b.link + "?token=" + url.QueryEscape(token)
	b.logger.Info("Sent Telegram link", "telegram_id", m.From.ID)
	b.sendHTML(ctx, m.Chat.ID, "This Telegram account is not linked to a hub user yet.\n"+
		`Open <a href="`+escapeHTML(link)+`">this link</a> and sign in to link it. `+
		"The link works once and expires in a few minutes. Then send your message again.", nil)
}

func (b *Bot) help(ctx context.Context, chatID int64, user string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Signed in as <b>%s</b>.\n", escapeHTML(user))
	if st := b.chat(chatID); st.att != nil && st.user == user {
		fmt.Fprintf(&sb, "Attached to session <code>%s</code> on <b>%s</b>.\n",
			escapeHTML(st.att.sessionID), escapeHTML(st.att.backendID))
	}
	sb.WriteString("\nSend a message to talk to Hermes.\n" +
		"/sessions list your sessions\n" +
		"/attach N attach to session N from /sessions\n" +
		"/new [backend] start a new session\n" +
		"/backends list your backends\n" +
		"/stop interrupt the current turn")
	b.sendHTML(ctx, chatID, sb.String(), nil)
}

func (b *Bot) backends(ctx context.Context, chatID int64, user string) ([]hub.Backend, bool) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	list, err := b.svc.Backends(cctx, user)
	if err != nil {
		b.reportErr(ctx, chatID, user, "list backends", err)
		return nil, false
	}
	if len(list) == 0 {
		b.sendText(ctx, chatID, "You have no backends.")
		return nil, false
	}
	return list, true
}

func (b *Bot) listBackends(ctx context.Context, chatID int64, user string) {
	list, ok := b.backends(ctx, chatID, user)
	if !ok {
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Backends</b>")
	for _, be := range list {
		fmt.Fprintf(&sb, "\n<code>%s</code> %s", escapeHTML(be.ID), escapeHTML(be.Name))
		if b.defaults[user] == be.ID {
			sb.WriteString(" <i>(default)</i>")
		}
	}
	b.sendHTML(ctx, chatID, sb.String(), nil)
}

func (b *Bot) listSessions(ctx context.Context, chatID int64, user string) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	list, err := b.svc.Sessions(cctx, user)
	if err != nil {
		b.reportErr(ctx, chatID, user, "list sessions", err)
		return
	}
	if len(list) == 0 {
		b.sendText(ctx, chatID, "No sessions yet. Send a message or /new to start one.")
		return
	}
	slices.SortStableFunc(list, func(x, y hub.Session) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
	list = list[:min(len(list), maxListed)]
	st := b.chat(chatID)
	st.listGen++
	st.list = list
	var sb strings.Builder
	sb.WriteString("<b>Sessions</b>")
	var rows [][]InlineButton
	for i, s := range list {
		title := s.Title
		if title == "" {
			title = untitledName
		}
		fmt.Fprintf(&sb, "\n%d. %s <i>(%s)</i>", i+1, escapeHTML(truncate(title, 60)), escapeHTML(s.BackendID))
		if s.Live {
			sb.WriteString(" live")
		}
		if st.att != nil && st.att.sessionID == s.ID && st.att.backendID == s.BackendID {
			sb.WriteString(" <b>(attached)</b>")
		}
		rows = append(rows, []InlineButton{{
			Text:         truncate(fmt.Sprintf("%d. %s", i+1, title), 40),
			CallbackData: fmt.Sprintf("%s:%d:%d", cbAttach, st.listGen, i+1),
		}})
	}
	b.sendHTML(ctx, chatID, sb.String(), &InlineKeyboard{InlineKeyboard: rows})
}

func (b *Bot) attachCommand(ctx context.Context, chatID int64, user, arg string) {
	n, err := strconv.Atoi(arg)
	st := b.chat(chatID)
	if err != nil || n < 1 || n > len(st.list) {
		b.sendText(ctx, chatID, "Send /sessions, then /attach N with a number from that list.")
		return
	}
	b.attachListed(ctx, chatID, user, st.list[n-1])
}

func (b *Bot) attachListed(ctx context.Context, chatID int64, user string, s hub.Session) {
	if err := b.attach(ctx, chatID, user, s.BackendID, s.ID, 0); err != nil {
		b.reportErr(ctx, chatID, user, "attach", err)
		return
	}
	title := s.Title
	if title == "" {
		title = untitledName
	}
	b.sendHTML(ctx, chatID, "Attached to <b>"+escapeHTML(title)+"</b> on <b>"+escapeHTML(s.BackendID)+"</b>.", nil)
}

func (b *Bot) newSession(ctx context.Context, chatID int64, user, arg string) {
	backendID, ok := b.pickBackend(ctx, chatID, user, arg)
	if !ok {
		return
	}
	s, err := b.startSession(ctx, chatID, user, backendID)
	if err != nil {
		b.reportErr(ctx, chatID, user, "start session", err)
		return
	}
	b.sendHTML(ctx, chatID, "New session on <b>"+escapeHTML(s.BackendID)+"</b>. Send a message to begin.", nil)
}

// pickBackend resolves arg (an ID or name) or the user's default.
func (b *Bot) pickBackend(ctx context.Context, chatID int64, user, arg string) (string, bool) {
	if arg == "" {
		if id, ok := b.defaults[user]; ok {
			return id, true
		}
	}
	list, ok := b.backends(ctx, chatID, user)
	if !ok {
		return "", false
	}
	if arg == "" {
		return list[0].ID, true
	}
	for _, be := range list {
		if strings.EqualFold(be.ID, arg) || strings.EqualFold(be.Name, arg) {
			return be.ID, true
		}
	}
	b.sendText(ctx, chatID, "Unknown backend. Send /backends to list yours.")
	return "", false
}

func (b *Bot) startSession(ctx context.Context, chatID int64, user, backendID string) (hub.Session, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	s, err := b.svc.NewSession(cctx, user, backendID)
	if err != nil {
		return hub.Session{}, fmt.Errorf("failed to create session: %w", err)
	}
	if err := b.attach(ctx, chatID, user, s.BackendID, s.ID, 0); err != nil {
		return hub.Session{}, err
	}
	return s, nil
}

// attach replaces the chat's attachment. The stream lives on ctx, the bot's
// run context.
func (b *Bot) attach(ctx context.Context, chatID int64, user, backendID, sessionID string, since int64) error {
	st := b.chat(chatID)
	if st.att != nil {
		st.att.stop()
		st.att = nil
	}
	actx, acancel := context.WithCancel(ctx)
	events, detach, err := b.svc.Attach(actx, user, backendID, sessionID, since)
	if err != nil {
		acancel()
		return fmt.Errorf("failed to attach: %w", err)
	}
	a := &attachment{
		ctx: actx, cancel: acancel, chatID: chatID, user: user, backendID: backendID, sessionID: sessionID,
		events: events, detach: detach, done: make(chan struct{}),
	}
	a.lastSeq.Store(since)
	st.att, st.user = a, user
	b.logger.Info("Attached Telegram chat", "chat", chatID, "user", user, "backend", backendID, "session", sessionID, "since", since)
	b.wg.Add(1)
	go b.pump(a)
	return nil
}

func (b *Bot) prompt(ctx context.Context, chatID int64, user, text string) {
	st := b.chat(chatID)
	a := st.att
	switch {
	case a == nil || st.user != user:
		backendID, ok := b.pickBackend(ctx, chatID, user, "")
		if !ok {
			return
		}
		if _, err := b.startSession(ctx, chatID, user, backendID); err != nil {
			b.reportErr(ctx, chatID, user, "start session", err)
			return
		}
	case a.closed():
		if err := b.attach(ctx, chatID, user, a.backendID, a.sessionID, a.lastSeq.Load()); err != nil {
			b.reportErr(ctx, chatID, user, "reattach", err)
			return
		}
	}
	a = st.att
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.svc.Prompt(cctx, user, a.backendID, a.sessionID, text); err != nil {
		b.reportErr(ctx, chatID, user, "prompt", err)
	}
}

func (b *Bot) stop(ctx context.Context, chatID int64, user string) {
	st := b.chat(chatID)
	if st.att == nil || st.user != user {
		b.sendText(ctx, chatID, "No session is attached.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.svc.Interrupt(cctx, user, st.att.backendID, st.att.sessionID); err != nil {
		b.reportErr(ctx, chatID, user, "interrupt", err)
		return
	}
	b.sendText(ctx, chatID, "Stop requested.")
}

func (b *Bot) pump(a *attachment) {
	defer b.wg.Done()
	defer close(a.done)
	ctx := a.ctx
	r := newRenderer(b.api, a.chatID, b.cfg.EditInterval, b.logger.With("chat", a.chatID, "session", a.sessionID),
		func(approvalID, text string) (string, func(int64)) {
			return b.addApproval(a, approvalID, text)
		})
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-a.events:
			if !ok {
				r.flushAll(ctx)
				b.logger.Info("Session stream ended", "chat", a.chatID, "session", a.sessionID)
				return
			}
			r.handle(ctx, ev)
			if ev.Seq > a.lastSeq.Load() {
				a.lastSeq.Store(ev.Seq)
			}
		case <-timer.C:
			r.flushDue(ctx)
		}
		if d, ok := r.nextFlush(); ok {
			timer.Reset(d)
		}
	}
}

func (b *Bot) addApproval(a *attachment, approvalID, text string) (string, func(int64)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextKey++
	key := strconv.FormatUint(b.nextKey, 36)
	p := &pendingApproval{chatID: a.chatID, backendID: a.backendID, sessionID: a.sessionID, approvalID: approvalID, text: text}
	b.approvals[key] = p
	return key, func(id int64) {
		b.mu.Lock()
		defer b.mu.Unlock()
		p.messageID = id
	}
}

func (b *Bot) handleCallback(ctx context.Context, q *CallbackQuery) {
	if q.Message == nil || !b.chatAllowed(q.Message.Chat) {
		b.answer(ctx, q.ID, "")
		return
	}
	chatID := q.Message.Chat.ID
	user, linked, err := b.lookupUser(ctx, q.From.ID)
	if err != nil {
		b.answer(ctx, q.ID, errGeneric)
		return
	}
	if !linked {
		b.answer(ctx, q.ID, "Link your Telegram account first: send me a direct message.")
		return
	}
	parts := strings.Split(q.Data, ":")
	switch {
	case len(parts) == 3 && parts[0] == cbApproval:
		b.answerApproval(ctx, q, user, parts[1], parts[2] == "1")
	case len(parts) == 3 && parts[0] == cbAttach:
		st := b.chat(chatID)
		gen, err1 := strconv.Atoi(parts[1])
		n, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || gen != st.listGen || n < 1 || n > len(st.list) {
			b.answer(ctx, q.ID, "That list is out of date. Send /sessions again.")
			return
		}
		b.answer(ctx, q.ID, "")
		b.attachListed(ctx, chatID, user, st.list[n-1])
	default:
		b.answer(ctx, q.ID, "")
	}
}

func (b *Bot) answerApproval(ctx context.Context, q *CallbackQuery, user, key string, allow bool) {
	b.mu.Lock()
	p, ok := b.approvals[key]
	var pc pendingApproval
	if ok {
		pc = *p
	}
	b.mu.Unlock()
	if !ok || pc.chatID != q.Message.Chat.ID {
		b.answer(ctx, q.ID, "This request is no longer pending.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	err := b.svc.RespondApproval(cctx, user, pc.backendID, pc.sessionID, pc.approvalID, allow)
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Approval answer forbidden", "user", user, "backend", pc.backendID)
		b.answer(ctx, q.ID, "You are not allowed to answer this request.")
		return
	case errors.Is(err, hub.ErrUnknownApproval):
		b.dropApproval(key)
		b.answer(ctx, q.ID, "This request is no longer pending.")
		b.editHTML(ctx, pc.chatID, q.Message.MessageID, pc.text+"\n\n<i>No longer pending.</i>")
		return
	case err != nil:
		b.logger.Error("Failed to answer approval", "user", user, "error", err)
		b.answer(ctx, q.ID, errGeneric)
		return
	}
	b.dropApproval(key)
	verdict := "Denied"
	if allow {
		verdict = "Allowed"
	}
	b.answer(ctx, q.ID, verdict)
	b.editHTML(ctx, pc.chatID, q.Message.MessageID, pc.text+"\n\n<i>"+verdict+" by "+escapeHTML(user)+".</i>")
}

func (b *Bot) dropApproval(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.approvals, key)
}

func (b *Bot) reportErr(ctx context.Context, chatID int64, user, action string, err error) {
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Hub denied Telegram request", "user", user, "action", action, "error", err)
		b.sendText(ctx, chatID, "You do not have access to that backend.")
	case errors.Is(err, hub.ErrUnknownBackend):
		b.sendText(ctx, chatID, "Unknown backend. Send /backends to list yours.")
	default:
		b.logger.Error("Telegram request failed", "user", user, "action", action, "error", err)
		b.sendText(ctx, chatID, errGeneric)
	}
}

func (b *Bot) answer(ctx context.Context, id, text string) {
	if err := b.api.AnswerCallback(ctx, id, text); err != nil {
		b.logger.Warn("Failed to answer callback", "error", err)
	}
}

func (b *Bot) sendText(ctx context.Context, chatID int64, text string) {
	if _, err := b.api.SendText(ctx, chatID, text); err != nil {
		b.logger.Warn("Failed to send message", "chat", chatID, "error", err)
	}
}

func (b *Bot) sendHTML(ctx context.Context, chatID int64, text string, kb *InlineKeyboard) {
	sendHTML(ctx, b.api, b.logger, chatID, text, kb)
}

func (b *Bot) editHTML(ctx context.Context, chatID, messageID int64, text string) {
	if err := b.api.EditHTML(ctx, chatID, messageID, text, nil); err != nil {
		b.logger.Warn("Failed to edit message", "chat", chatID, "message", messageID, "error", err)
	}
}

// sendHTML sends text as HTML and returns the message ID, or 0 on failure.
// When Telegram rejects the markup it resends the text without tags.
func sendHTML(ctx context.Context, api *Client, logger *slog.Logger, chatID int64, text string, kb *InlineKeyboard) int64 {
	id, err := api.SendHTML(ctx, chatID, text, kb)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.BadEntities() {
		logger.Warn("Telegram rejected HTML, sending plain text", "chat", chatID, "error", err)
		plain := html.UnescapeString(tagRe.ReplaceAllString(text, ""))
		var m Message
		err = api.call(ctx, "sendMessage", sendMessageParams{
			ChatID: chatID, Text: plain, ReplyMarkup: kb, LinkPreview: &linkPreview{IsDisabled: true},
		}, &m)
		id = m.MessageID
	}
	if err != nil {
		logger.Warn("Failed to send message", "chat", chatID, "error", err)
		return 0
	}
	return id
}
