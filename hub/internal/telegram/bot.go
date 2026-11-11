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
	"github.com/google/uuid"
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
	msgHeld      = "Hermes is restarting. Your message will send when it is back."
)

var tagRe = regexp.MustCompile(`<[^>]*>`)

// ErrMissingPublicURL is returned by New without Options.PublicURL.
var ErrMissingPublicURL = errors.New("telegram bot needs the hub public URL")

// Options holds the Bot's dependencies.
type Options struct {
	// PublicURL is the hub's public base URL (http.public_url); link
	// messages point at PublicURL + /link/telegram.
	PublicURL string
	Service   hub.Service
	Links     hub.Links
	Tokens    hub.TokenStore
	// State keeps chat attachments and the update offset, so a replica
	// that takes the bot over carries on. Optional.
	State hub.TelegramState
	// Computers lets users start and move chats on their computers.
	// Optional.
	Computers  hub.Computers
	Logger     *slog.Logger
	HTTPClient *http.Client
}

// BotID is the numeric bot ID at the start of a bot token, or 0.
func BotID(token string) int64 {
	head, _, _ := strings.Cut(token, ":")
	id, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// Bot is the Telegram bot.
type Bot struct {
	cfg       Config
	link      string
	api       *Client
	svc       hub.Service
	links     hub.Links
	tokens    hub.TokenStore
	state     hub.TelegramState
	computers hub.Computers
	botID     int64
	logger    *slog.Logger
	defaults  map[string]string
	groups    map[int64]bool

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
	release *pendingRelease
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
		state:     opts.State,
		computers: opts.Computers,
		botID:     BotID(cfg.Token),
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
// Only one process may poll a bot token at a time. With Options.State it
// starts from the stored update offset and attaches every stored chat
// again, so Run may move between replicas. When ctx ends, the update being
// handled is finished and the rest of its batch is left for the next
// leader.
func (b *Bot) Run(ctx context.Context) error {
	b.chats = make(map[int64]*chatState)
	defer b.shutdown()
	offset := b.restore(ctx)
	hctx := context.WithoutCancel(ctx)
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
		updates = b.completeAlbum(ctx, offset, updates)
		for i := 0; i < len(updates); i++ {
			if ctx.Err() != nil {
				break
			}
			u := updates[i]
			sctx := hub.WithSubmissionID(hctx, b.submissionID(u.UpdateID))
			if album := albumAt(updates, i); len(album) > 1 {
				i += len(album) - 1
				b.handleAlbum(sctx, album)
			} else {
				b.handleUpdate(sctx, u)
			}
			offset = updates[i].UpdateID + 1
			b.saveOffset(hctx, offset)
		}
	}
	return nil
}

// albumWait is how long the bot waits for the rest of an album whose
// last message ended a batch of updates.
const albumWait = 2 * time.Second

// completeAlbum adds the rest of an album to a batch that ends inside one:
// Telegram sends each photo of an album as its own message, and they may
// arrive in separate batches. It polls from the batch's own offset, which
// confirms nothing new to Telegram, so a leader that stops before it
// handled the batch leaves all of it to the next one.
func (b *Bot) completeAlbum(ctx context.Context, offset int64, updates []Update) []Update {
	for range 3 {
		if len(updates) == 0 {
			return updates
		}
		last := updates[len(updates)-1]
		if last.Message == nil || last.Message.MediaGroupID == "" {
			return updates
		}
		pctx, cancel := context.WithTimeout(ctx, albumWait+pollSlack)
		more, err := b.api.GetUpdates(pctx, offset, albumWait)
		cancel()
		if err != nil {
			return updates
		}
		n := len(updates)
		for _, u := range more {
			if u.UpdateID > last.UpdateID {
				updates = append(updates, u)
			}
		}
		if len(updates) == n {
			return updates
		}
	}
	return updates
}

// albumAt is the run of messages of one album in one chat that starts at
// updates[i]. It is empty when updates[i] is not part of an album.
func albumAt(updates []Update, i int) []*Message {
	first := updates[i].Message
	if first == nil || first.MediaGroupID == "" {
		return nil
	}
	out := []*Message{first}
	for _, u := range updates[i+1:] {
		m := u.Message
		if m == nil || m.MediaGroupID != first.MediaGroupID || m.Chat.ID != first.Chat.ID {
			break
		}
		out = append(out, m)
	}
	return out
}

// submissionID derives the Hermes submission ID of an update's prompt, so
// a replica that handles the update again after a takeover or a crash
// sends the same ID and Hermes runs the prompt once.
func (b *Bot) submissionID(updateID int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "hermes-hub:telegram:%d:%d", b.botID, updateID)).String()
}

// restore loads the stored update offset and attaches every stored chat
// again, from a fresh snapshot. A chat whose session cannot be attached
// now attaches again on its next message.
func (b *Bot) restore(ctx context.Context) int64 {
	if b.state == nil {
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	offset, err := b.state.TelegramOffset(cctx, b.botID)
	if err != nil {
		b.logger.Error("Failed to load the Telegram update offset", "error", err)
	}
	chats, err := b.state.TelegramChats(cctx)
	if err != nil {
		b.logger.Error("Failed to load Telegram chats", "error", err)
	}
	for _, c := range chats {
		if err := b.attach(ctx, c.ChatID, c.User, c.BackendID, c.SessionID, 0); err != nil {
			b.logger.Warn("Failed to attach a stored Telegram chat, it attaches on its next message",
				"chat", c.ChatID, "session", c.SessionID, "error", err)
			st := b.chat(c.ChatID)
			st.user, st.att = c.User, detached(c)
		}
	}
	if len(chats) > 0 {
		b.logger.Info("Restored Telegram chats", "chats", len(chats))
	}
	return offset
}

// detached is an attachment that ended, so the chat's next message
// attaches its session again.
func detached(c hub.TelegramChat) *attachment {
	a := &attachment{chatID: c.ChatID, user: c.User, backendID: c.BackendID, sessionID: c.SessionID,
		cancel: func() {}, detach: func() {}, done: make(chan struct{})}
	close(a.done)
	return a
}

func (b *Bot) saveOffset(ctx context.Context, offset int64) {
	if b.state == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.state.SetTelegramOffset(cctx, b.botID, offset); err != nil {
		b.logger.Error("Failed to store the Telegram update offset", "error", err)
	}
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
	b.handleAlbum(ctx, []*Message{m})
}

// handleAlbum handles one message, or the messages of one album as one
// prompt.
func (b *Bot) handleAlbum(ctx context.Context, ms []*Message) {
	m := ms[0]
	media := slices.ContainsFunc(ms, hasMedia)
	if m.From == nil || m.From.IsBot || m.Text == "" && !media || !b.chatAllowed(m.Chat) {
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
	if media {
		b.promptMedia(ctx, m.Chat.ID, user, ms)
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
	case "devices", "computers":
		b.listComputers(ctx, m.Chat.ID, user)
	case "where":
		b.where(ctx, m.Chat.ID, user)
	case "move":
		b.move(ctx, m.Chat.ID, user, arg)
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
	if b.computers != nil {
		sb.WriteString("\n/devices list your computers\n" +
			"/new NAME [folder] start a session on a computer\n" +
			"/where show where this session runs\n" +
			"/move NAME [folder] run this session on a computer\n" +
			"/move off stop using its computer")
	}
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
	list = slices.DeleteFunc(list, func(s hub.Session) bool { return s.Archived })
	down := b.unreachable(cctx, user)
	if len(list) == 0 {
		if len(down) > 0 {
			b.sendText(ctx, chatID, "Can't reach "+strings.Join(down, ", ")+" right now. It may be restarting; try again in a minute.")
			return
		}
		b.sendText(ctx, chatID, "No sessions yet. Send a message or /new to start one.")
		return
	}
	slices.SortStableFunc(list, func(x, y hub.Session) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
	list = list[:min(len(list), maxListed)]
	st := b.chat(chatID)
	st.listGen++
	st.list = list
	places := b.places(cctx, user, list)
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
		if from := sourceName(s.Source); from != "" {
			sb.WriteString(", from " + from)
		}
		p, ok := places[hub.SessionRef{BackendID: s.BackendID, SessionID: s.ID}]
		sb.WriteString(placeTag(p, ok))
		if st.att != nil && st.att.sessionID == s.ID && st.att.backendID == s.BackendID {
			sb.WriteString(" <b>(attached)</b>")
		}
		rows = append(rows, []InlineButton{{
			Text:         truncate(fmt.Sprintf("%d. %s", i+1, title), 40),
			CallbackData: fmt.Sprintf("%s:%d:%d", cbAttach, st.listGen, i+1),
		}})
	}
	if len(down) > 0 {
		fmt.Fprintf(&sb, "\n\n<i>Can't reach %s right now, so its sessions are missing.</i>", escapeHTML(strings.Join(down, ", ")))
	}
	b.sendHTML(ctx, chatID, sb.String(), &InlineKeyboard{InlineKeyboard: rows})
}

// places looks up where the listed sessions run, when computers are on.
func (b *Bot) places(ctx context.Context, user string, list []hub.Session) map[hub.SessionRef]hub.Place {
	if b.computers == nil {
		return nil
	}
	refs := make([]hub.SessionRef, 0, len(list))
	for _, s := range list {
		refs = append(refs, hub.SessionRef{BackendID: s.BackendID, SessionID: s.ID})
	}
	return b.computers.Places(ctx, user, refs)
}

// sourceName names the surface that started a session, for one that was
// not started here or in the web app.
func sourceName(source string) string {
	switch source {
	case "acp":
		return "an editor"
	case "cli", "tui":
		return "the terminal"
	}
	return ""
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
	if arg != "" && !b.isBackend(ctx, user, arg) && b.newOnComputer(ctx, chatID, user, arg) {
		return
	}
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

// isBackend reports whether arg names one of user's backends.
func (b *Bot) isBackend(ctx context.Context, user, arg string) bool {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	list, err := b.svc.Backends(cctx, user)
	if err != nil {
		return true
	}
	for _, be := range list {
		if strings.EqualFold(be.ID, arg) || strings.EqualFold(be.Name, arg) {
			return true
		}
	}
	return false
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
	if b.computers != nil {
		b.sendText(ctx, chatID, "Unknown backend or computer. Send /backends or /devices to list yours.")
		return "", false
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
	if b.state != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
		defer cancel()
		err := b.state.SaveTelegramChat(cctx, hub.TelegramChat{ChatID: chatID, User: user, BackendID: backendID, SessionID: sessionID})
		if err != nil {
			b.logger.Error("Failed to store the Telegram chat", "chat", chatID, "error", err)
		}
	}
	return nil
}

func (b *Bot) prompt(ctx context.Context, chatID int64, user, text string, files ...hub.Attachment) {
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
		err := b.attach(ctx, chatID, user, a.backendID, a.sessionID, a.lastSeq.Load())
		if errors.Is(err, hub.ErrUnknownSession) {
			_, err = b.startSession(ctx, chatID, user, a.backendID)
		}
		if err != nil {
			b.reportErr(ctx, chatID, user, "reattach", err)
			return
		}
	}
	a = st.att
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := b.svc.Prompt(cctx, user, a.backendID, a.sessionID, text, files...)
	switch {
	case err != nil:
		b.reportErr(ctx, chatID, user, "prompt", err)
	case res.Held:
		b.sendText(ctx, chatID, msgHeld)
	}
	if err == nil && res.Notice != "" {
		b.sendText(ctx, chatID, res.Notice)
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
	if m, ok := b.svc.(hub.Media); ok {
		r.media = func(ctx context.Context, p string) (*hub.MediaFile, error) {
			return m.Media(ctx, a.user, a.backendID, a.sessionID, p)
		}
	}
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
	case len(parts) == 2 && parts[0] == cbRelease && b.computers != nil:
		b.confirmRelease(ctx, q, user, parts[1])
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
	if b.computerErr(ctx, chatID, err) {
		b.logger.Info("Telegram computer request refused", "user", user, "action", action, "error", err)
		return
	}
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Hub denied Telegram request", "user", user, "action", action, "error", err)
		b.sendText(ctx, chatID, "You do not have access to that backend.")
	case errors.Is(err, hub.ErrUnknownBackend):
		b.sendText(ctx, chatID, "Unknown backend. Send /backends to list yours.")
	case errors.Is(err, hub.ErrShuttingDown):
		b.sendText(ctx, chatID, "The hub is restarting. Send your message again in a moment.")
	case errors.Is(err, hub.ErrAttachment):
		b.sendText(ctx, chatID, hub.AttachmentReason(err)+".")
	case errors.Is(err, hub.ErrHoldFull):
		b.sendText(ctx, chatID, "Too many messages are waiting for Hermes to come back. Send this one again later.")
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

// statusReporter is the optional part of a hub.Service that reports
// backend connection errors.
type statusReporter interface {
	BackendStatus(ctx context.Context, user string) (map[string]string, error)
}

// unreachable names user's backends whose last connection attempt failed.
func (b *Bot) unreachable(ctx context.Context, user string) []string {
	sr, ok := b.svc.(statusReporter)
	if !ok {
		return nil
	}
	status, err := sr.BackendStatus(ctx, user)
	if err != nil {
		return nil
	}
	backends, err := b.svc.Backends(ctx, user)
	if err != nil {
		return nil
	}
	var names []string
	for _, be := range backends {
		if status[be.ID] != "" {
			name := be.Name
			if name == "" {
				name = be.ID
			}
			names = append(names, name)
		}
	}
	return names
}
