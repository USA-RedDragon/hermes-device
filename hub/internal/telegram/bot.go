// Package telegram is the hub's Telegram front end. It long-polls the Bot
// API, links each Telegram account to a hub user, and relays that user's
// Hermes sessions into the chat.
package telegram

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/google/uuid"
)

const (
	callTimeout = 30 * time.Second
	maxBackoff  = 30 * time.Second
	maxListed   = 10
	pollSlack   = 15 * time.Second
	chatPrivate = "private"
	chatGroup   = "group"
	chatSuper   = "supergroup"
	errGeneric  = "Something went wrong. Try again later."
	linkPath    = "/link/telegram"
	// cbApproval prefixes the prompt buttons of an attached chat. It is not
	// the "ap" of older hubs, whose buttons meant allow and deny.
	cbApproval = "pa"
	cbAttach   = "at"
	// cbSteps is the Steps button of a running turn's status line.
	cbSteps      = "ss"
	untitledName = "(untitled)"
	cmdLink      = "link"
	cmdSkip      = "skip"
	msgHeld      = "The agent is restarting. Your message will send when it is back."
)

var tagRe = regexp.MustCompile(`<[^>]*>`)

// dest is where the bot writes: a chat, and in a forum group or a private
// chat with topics a topic. A thread of 0 is a private chat or its main
// thread, a plain group or a forum's General topic.
type dest struct {
	chat, thread int64
}

// msgKey names one message.
type msgKey struct {
	chat, msg int64
}

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
	Computers hub.Computers
	// Notices finds the notices whose buttons people press. Optional.
	Notices NoticeStore
	// Claude signs Claude accounts in from Telegram. Optional.
	Claude hub.Claude
	// Subagents sets a chat's Subagents choice from /model. Optional.
	Subagents hub.SubagentRoutes
	// Prompts lists the saved prompts for /prompts. Optional.
	Prompts PromptStore
	// Pins puts each user's pinned chats first in /sessions. Optional.
	Pins       PinStore
	Logger     *slog.Logger
	HTTPClient *http.Client
	// statusTick replaces the package's statusTick in tests.
	statusTick time.Duration
	// editGaps replaces the edit gaps of private chats and groups in
	// tests.
	editGaps [2]time.Duration
}

// PinStore is the part of hub.ChatStore the bot reads.
type PinStore interface {
	ChatMarks(ctx context.Context, user string) (map[hub.SessionRef]hub.ChatMark, time.Time, error)
}

// NoticeStore is the part of hub.NotifyStore the bot reads.
type NoticeStore interface {
	Notice(ctx context.Context, id string) (hub.NoticeRecord, bool, error)
	NoticeMessages(ctx context.Context, id string) ([]hub.NoticeMessage, error)
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
	public    string
	api       *Client
	svc       hub.Service
	links     hub.Links
	tokens    hub.TokenStore
	state     hub.TelegramState
	computers hub.Computers
	notices   NoticeStore
	claude    hub.Claude
	subagents hub.SubagentRoutes
	prompts   PromptStore
	pins      PinStore
	botID     int64
	logger    *slog.Logger
	defaults  map[string]string
	groups    map[int64]bool
	now       func() time.Time
	// tick is how often a status line shows the time again (statusTick).
	tick time.Duration

	// loop serializes the handling of updates with the topic sync; it
	// guards chats.
	loop  sync.Mutex
	chats map[dest]*chatState
	wg    sync.WaitGroup

	// fmu guards forums, the linked groups by chat ID.
	fmu    sync.Mutex
	forums map[int64]*forum

	mu        sync.Mutex
	approvals map[string]*pendingApproval
	// picks are the toggled choices of multi-select questions, by
	// promptTarget.id.
	picks map[string][]bool
	// replies are the messages a reply to which answers a question.
	replies map[msgKey]replyTarget
	// shown are the texts of sent notices, to show their outcome.
	shown map[msgKey]shownNotice
	// gone are topics Telegram refused a message for.
	gone map[dest]bool
	// claudes are the Claude sign-in messages whose reply is the code.
	claudes map[msgKey]claudePrompt
	// steps are the steps so far of each running turn, by its status
	// message, for that message's Steps button.
	steps map[msgKey]string
	// deleted are topics whose chat was deleted; the next sync deletes
	// them.
	deleted map[dest]bool
	// sources are the surfaces that started the chats the bot listed, to
	// say where a prompt from elsewhere came from.
	sources map[hub.SessionRef]string
	// sent are the Telegram messages of the prompts the bot sent, by
	// submission ID, for the reactions of their turns.
	sent map[string]sentPrompt
	// budget spaces the edits of each chat across its topics.
	budget *budget
	// me is what getMe last said of the bot; meAt is when.
	me   atomic.Pointer[Me]
	meAt atomic.Int64
}

// sentPrompt is the message of a prompt the bot sent.
type sentPrompt struct {
	msg int64
	at  time.Time
}

// notePrompt keeps the message of a prompt the bot sends under id.
func (b *Bot) notePrompt(id string, msg int64) {
	if msg == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for k, p := range b.sent {
		if now.Sub(p.at) > keepShown {
			delete(b.sent, k)
		}
	}
	b.sent[id] = sentPrompt{msg: msg, at: now}
}

// sentMessage is the message of the prompt the bot sent under id.
func (b *Bot) sentMessage(id string) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.sent[id]
	return p.msg, ok
}

type chatState struct {
	user    string
	att     *attachment
	listGen int
	list    []hub.Session
	release *pendingRelease
	models  modelMenu
	prompts promptMenu
}

type attachment struct {
	ctx       context.Context //nolint:containedctx // lives as long as the attachment; cancelled by stop
	cancel    context.CancelFunc
	to        dest
	user      string
	backendID string
	sessionID string
	events    <-chan hub.Event
	detach    func()
	done      chan struct{}
	lastSeq   atomic.Int64
	once      sync.Once
	status    sessionStatus
	// lastEvent is when the stream last had an event, in Unix nanoseconds,
	// and prompts how many prompts it shows; the topic sync reads both.
	lastEvent atomic.Int64
	prompts   atomic.Int64
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
	to        dest
	messageID int64
	backendID string
	sessionID string
	prompt    hub.PromptInfo
	// body is the message's HTML under its head, kept to show the outcome.
	body string
	// bound is closed once messageID is set.
	bound chan struct{}
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
		public:    strings.TrimRight(opts.PublicURL, "/"),
		api:       NewClient(cfg.APIURL, cfg.Token, opts.HTTPClient, cfg.MaxRetries, logger),
		svc:       opts.Service,
		links:     opts.Links,
		tokens:    opts.Tokens,
		state:     opts.State,
		computers: opts.Computers,
		notices:   opts.Notices,
		claude:    opts.Claude,
		subagents: opts.Subagents,
		prompts:   opts.Prompts,
		pins:      opts.Pins,
		botID:     BotID(cfg.Token),
		logger:    logger,
		defaults:  defaults,
		groups:    make(map[int64]bool, len(cfg.AllowedGroups)),
		now:       time.Now,
		tick:      cmp.Or(opts.statusTick, statusTick),
		chats:     make(map[dest]*chatState),
		forums:    make(map[int64]*forum),
		approvals: make(map[string]*pendingApproval), steps: make(map[msgKey]string), deleted: make(map[dest]bool),
		picks:   make(map[string][]bool),
		replies: make(map[msgKey]replyTarget),
		shown:   make(map[msgKey]shownNotice),
		gone:    make(map[dest]bool),
		claudes: make(map[msgKey]claudePrompt),
		sources: make(map[hub.SessionRef]string),
		sent:    make(map[string]sentPrompt),
	}
	b.budget = newBudget(func() time.Time { return b.now() })
	if opts.editGaps[0] > 0 {
		b.budget.private, b.budget.group = opts.editGaps[0], opts.editGaps[1]
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
	b.loop.Lock()
	b.chats = make(map[dest]*chatState)
	offset := b.restore(ctx)
	b.loop.Unlock()
	defer b.shutdown()
	syncDone := make(chan struct{})
	go b.syncTopics(ctx, syncDone)
	defer func() { <-syncDone }()
	b.setCommands(ctx)
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
			b.loop.Lock()
			if album := albumAt(updates, i); len(album) > 1 {
				i += len(album) - 1
				b.handleAlbum(sctx, album)
			} else {
				b.handleUpdate(sctx, u)
			}
			b.loop.Unlock()
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
		to := dest{chat: c.ChatID}
		if err := b.attach(ctx, to, c.User, c.BackendID, c.SessionID, 0); err != nil {
			b.logger.Warn("Failed to attach a stored Telegram chat, it attaches on its next message",
				"chat", c.ChatID, "session", c.SessionID, "error", err)
			st := b.chat(to)
			st.user, st.att = c.User, detached(to, c.User, c.BackendID, c.SessionID)
		}
	}
	if len(chats) > 0 {
		b.logger.Info("Restored Telegram chats", "chats", len(chats))
	}
	b.restoreForums(cctx)
	return offset
}

// detached is an attachment that ended, so the chat's next message
// attaches its session again.
func detached(to dest, user, backendID, sessionID string) *attachment {
	a := &attachment{to: to, user: user, backendID: backendID, sessionID: sessionID,
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
	b.loop.Lock()
	defer b.loop.Unlock()
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

// chat is the state of a chat or topic. b.loop is held.
func (b *Bot) chat(to dest) *chatState {
	st, ok := b.chats[to]
	if !ok {
		st = &chatState{}
		b.chats[to] = st
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
	if m.From == nil || m.From.IsBot || m.Text == "" && !media {
		return
	}
	if m.Chat.Type == chatPrivate {
		b.refreshMe(ctx)
	}
	if f := b.forum(m.Chat.ID); f != nil {
		if !isDM(f.chatID) || (b.dmTopics() || b.me.Load() == nil) && b.dmOwner(ctx, f, m.From.ID) {
			b.handleForum(ctx, f, ms, media)
			return
		}
		b.dropDM(ctx, f)
	}
	if cmd, _ := parseCommand(m.Text); cmd == cmdLink && m.Chat.Type != chatPrivate {
		b.linkGroup(ctx, m)
		return
	}
	if !b.chatAllowed(m.Chat) {
		return
	}
	to := dest{chat: m.Chat.ID}
	user, linked, err := b.lookupUser(ctx, m.From.ID)
	if err != nil {
		b.sendText(ctx, to, errGeneric)
		return
	}
	if !linked {
		b.sendLink(ctx, m)
		return
	}
	if m.Chat.Type == chatPrivate {
		if f := b.dmForum(ctx, m.Chat.ID, user); f != nil {
			b.handleForum(ctx, f, ms, media)
			return
		}
	}
	if !media && (b.claudeReply(ctx, to, user, m) || b.replyAnswer(ctx, to, user, m)) {
		return
	}
	if media {
		b.promptMedia(ctx, to, user, ms)
		return
	}
	cmd, arg := parseCommand(m.Text)
	b.logger.Debug("Telegram message", "user", user, "chat", m.Chat.ID, "command", cmd)
	switch cmd {
	case "":
		b.prompt(ctx, to, user, m.MessageID, m.Text)
	case "start", cmdHelp:
		b.help(ctx, to, user)
	case cmdBackends:
		b.listBackends(ctx, to, user)
	case cmdSessions:
		b.listSessions(ctx, to, user)
	case "attach":
		b.attachCommand(ctx, to, user, arg)
	case cmdNew:
		b.newSession(ctx, to, user, arg)
	case cmdLink:
		b.sendText(ctx, to, "This account is linked. To get a topic for each chat, send /link in a group with Topics.")
	case cmdUnlink:
		b.unlinkFromDM(ctx, to, user)
	default:
		b.sessionCommand(ctx, to, user, cmd, arg)
	}
}

// sessionCommand handles the commands that act on the chat's session.
func (b *Bot) sessionCommand(ctx context.Context, to dest, user, cmd, arg string) {
	switch cmd {
	case cmdStop:
		b.stop(ctx, to, user)
	case cmdStatus:
		b.status(ctx, to, user)
	case cmdDevices, "device", "computers":
		b.listComputers(ctx, to, user)
	case cmdPrompts:
		b.listPrompts(ctx, to, user)
	case cmdModel:
		b.model(ctx, to, user, arg)
	case cmdBg, "background":
		b.background(ctx, to, user, arg)
	case cmdCompress, "compact":
		b.compress(ctx, to, user, arg)
	case cmdWhere:
		b.where(ctx, to, user)
	case cmdMove:
		b.move(ctx, to, user, arg)
	case "cancel", cmdSkip:
		b.sendText(ctx, to, "Nothing to cancel. To answer a question, reply to its message.")
	default:
		b.sendText(ctx, to, "Unknown command. Send /help for the list.")
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
	to := dest{chat: m.Chat.ID}
	if m.Chat.Type != chatPrivate {
		b.sendText(ctx, to, "Send me a direct message to link your Telegram account first.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	token, err := b.tokens.CreateLinkToken(cctx, m.From.ID)
	if err != nil {
		b.logger.Error("Failed to create link token", "telegram_id", m.From.ID, "error", err)
		b.sendText(ctx, to, errGeneric)
		return
	}
	link := b.link + "?token=" + url.QueryEscape(token)
	b.logger.Info("Sent Telegram link", "telegram_id", m.From.ID)
	b.sendHTML(ctx, to, "This Telegram account is not linked to a hub user yet.\n"+
		`Open <a href="`+escapeHTML(link)+`">this link</a> and sign in to link it. `+
		"The link works once and expires in a few minutes. Then send your message again.", nil)
}

func (b *Bot) help(ctx context.Context, to dest, user string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Signed in as <b>%s</b>.\n", escapeHTML(user))
	if st := b.chat(to); st.att != nil && st.user == user {
		fmt.Fprintf(&sb, "Attached to session <code>%s</code> on <b>%s</b>.\n",
			escapeHTML(st.att.sessionID), escapeHTML(st.att.backendID))
	}
	sb.WriteString("\nSend a message to talk to Talaria. Reply to a question's message to answer it.\n" +
		"/sessions list your sessions\n" +
		"/attach N attach to session N from /sessions\n" +
		"/new [backend] start a new session\n" +
		"/backends list your backends\n" +
		"/stop interrupt the current turn\n" +
		"/status show the model and how full its context is\n" +
		"/model show or switch the model\n" +
		"/bg TEXT run a prompt in the background")
	if b.prompts != nil {
		sb.WriteString("\n/prompts get the text of a saved prompt")
	}
	if b.computers != nil {
		sb.WriteString("\n/devices list your computers\n" +
			"/new NAME [folder] start a session on a computer\n" +
			"/where show where this session runs\n" +
			"/move NAME [folder] run this session on a computer\n" +
			"/move off stop using its computer")
	}
	sb.WriteString("\n\nFor a topic per chat, make a group with Topics, add me as an admin who may manage topics, and send /link there.")
	b.sendHTML(ctx, to, sb.String(), nil)
}

func (b *Bot) backends(ctx context.Context, to dest, user string) ([]hub.Backend, bool) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	list, err := b.svc.Backends(cctx, user)
	if err != nil {
		b.reportErr(ctx, to, user, "list backends", err)
		return nil, false
	}
	if len(list) == 0 {
		b.sendText(ctx, to, "You have no backends.")
		return nil, false
	}
	return list, true
}

func (b *Bot) listBackends(ctx context.Context, to dest, user string) {
	list, ok := b.backends(ctx, to, user)
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
	b.sendHTML(ctx, to, sb.String(), nil)
}

func (b *Bot) listSessions(ctx context.Context, to dest, user string) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	list, err := b.svc.Sessions(cctx, user)
	if err != nil && hub.Unreachable(err) == nil {
		b.reportErr(ctx, to, user, "list sessions", err)
		return
	}
	b.noteSources(list)
	list = slices.DeleteFunc(list, func(s hub.Session) bool { return s.Archived })
	down := b.unreachable(cctx, user)
	if len(list) == 0 {
		if len(down) > 0 {
			b.sendText(ctx, to, "Can't reach "+strings.Join(down, ", ")+" right now. It may be restarting; try again in a minute.")
			return
		}
		b.sendText(ctx, to, "No sessions yet. Send a message or /new to start one.")
		return
	}
	marks := b.pinMarks(cctx, user)
	hub.PinnedFirst(list, marks)
	list = list[:min(len(list), maxListed)]
	st := b.chat(to)
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
		if marks[hub.SessionRef{BackendID: s.BackendID, SessionID: s.ID}].Pinned {
			sb.WriteString(" pinned")
		}
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
	b.sendHTML(ctx, to, sb.String(), &InlineKeyboard{InlineKeyboard: rows})
}

// pinMarks reads the user's pins; without a pin store, or when it fails,
// no chat is pinned.
func (b *Bot) pinMarks(ctx context.Context, user string) map[hub.SessionRef]hub.ChatMark {
	if b.pins == nil {
		return nil
	}
	marks, _, err := b.pins.ChatMarks(ctx, user)
	if err != nil {
		b.logger.Warn("Failed to read pins", "user", user, "error", err)
		return nil
	}
	return marks
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

// noteSources keeps the surfaces that started the listed chats.
func (b *Bot) noteSources(list []hub.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range list {
		b.sources[hub.SessionRef{BackendID: s.BackendID, SessionID: s.ID}] = s.Source
	}
}

// source is the surface that started a chat the bot listed, or "".
func (b *Bot) source(ref hub.SessionRef) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sources[ref]
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

func (b *Bot) attachCommand(ctx context.Context, to dest, user, arg string) {
	n, err := strconv.Atoi(arg)
	st := b.chat(to)
	if err != nil || n < 1 || n > len(st.list) {
		b.sendText(ctx, to, "Send /sessions, then /attach N with a number from that list.")
		return
	}
	b.attachListed(ctx, to, user, st.list[n-1])
}

func (b *Bot) attachListed(ctx context.Context, to dest, user string, s hub.Session) {
	if err := b.attach(ctx, to, user, s.BackendID, s.ID, 0); err != nil {
		b.reportErr(ctx, to, user, "attach", err)
		return
	}
	title := s.Title
	if title == "" {
		title = untitledName
	}
	b.sendHTML(ctx, to, "Attached to <b>"+escapeHTML(title)+"</b> on <b>"+escapeHTML(s.BackendID)+"</b>.", nil)
}

func (b *Bot) newSession(ctx context.Context, to dest, user, arg string) {
	if arg != "" && !b.isBackend(ctx, user, arg) {
		if s, p, matched, ok := b.startOnComputer(ctx, to, user, arg); matched {
			if !ok {
				return
			}
			if err := b.attach(ctx, to, user, s.BackendID, s.ID, 0); err != nil {
				b.reportErr(ctx, to, user, "attach", err)
				return
			}
			b.sendHTML(ctx, to, computerStarted(p), nil)
			return
		}
	}
	backendID, ok := b.pickBackend(ctx, to, user, arg)
	if !ok {
		return
	}
	s, err := b.startSession(ctx, to, user, backendID)
	if err != nil {
		b.reportErr(ctx, to, user, "start session", err)
		return
	}
	b.sendHTML(ctx, to, "New session on <b>"+escapeHTML(s.BackendID)+"</b>. Send a message to begin.", nil)
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
func (b *Bot) pickBackend(ctx context.Context, to dest, user, arg string) (string, bool) {
	if arg == "" {
		if id, ok := b.defaults[user]; ok {
			return id, true
		}
	}
	list, ok := b.backends(ctx, to, user)
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
		b.sendText(ctx, to, "Unknown backend or computer. Send /backends or /devices to list yours.")
		return "", false
	}
	b.sendText(ctx, to, "Unknown backend. Send /backends to list yours.")
	return "", false
}

func (b *Bot) startSession(ctx context.Context, to dest, user, backendID string) (hub.Session, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	s, err := b.svc.NewSession(cctx, user, backendID)
	if err != nil {
		return hub.Session{}, fmt.Errorf("failed to create session: %w", err)
	}
	if err := b.attach(ctx, to, user, s.BackendID, s.ID, 0); err != nil {
		return hub.Session{}, err
	}
	return s, nil
}

// attach replaces the chat's attachment. The stream lives on ctx, the bot's
// run context.
func (b *Bot) attach(ctx context.Context, to dest, user, backendID, sessionID string, since int64) error {
	st := b.chat(to)
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
		ctx: actx, cancel: acancel, to: to, user: user, backendID: backendID, sessionID: sessionID,
		events: events, detach: detach, done: make(chan struct{}),
	}
	a.lastSeq.Store(max(since, 0))
	a.lastEvent.Store(b.now().UnixNano())
	st.att, st.user = a, user
	b.logger.Info("Attached Telegram chat", "chat", to.chat, "thread", to.thread, "user", user, "backend", backendID, "session", sessionID, "since", since)
	b.wg.Add(1)
	go b.pump(a)
	if b.state != nil && b.forum(to.chat) == nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
		defer cancel()
		err := b.state.SaveTelegramChat(cctx, hub.TelegramChat{ChatID: to.chat, User: user, BackendID: backendID, SessionID: sessionID})
		if err != nil {
			b.logger.Error("Failed to store the Telegram chat", "chat", to.chat, "error", err)
		}
	}
	return nil
}

// prompt sends text as the chat's next input. msg is the Telegram message
// it came from, which gets the turn's reactions.
func (b *Bot) prompt(ctx context.Context, to dest, user string, msg int64, text string, files ...hub.Attachment) {
	st := b.chat(to)
	a := st.att
	switch {
	case a == nil || st.user != user:
		backendID, ok := b.pickBackend(ctx, to, user, "")
		if !ok {
			return
		}
		if _, err := b.startSession(ctx, to, user, backendID); err != nil {
			b.reportErr(ctx, to, user, "start session", err)
			return
		}
	case a.closed():
		err := b.attach(ctx, to, user, a.backendID, a.sessionID, a.lastSeq.Load())
		if errors.Is(err, hub.ErrUnknownSession) && to.thread != 0 {
			b.sendText(ctx, to, "This chat is gone from the hub. Send a message in "+generalName(to.chat)+" to start a new one.")
			return
		}
		if errors.Is(err, hub.ErrUnknownSession) {
			_, err = b.startSession(ctx, to, user, a.backendID)
		}
		if err != nil {
			b.reportErr(ctx, to, user, "reattach", err)
			return
		}
	}
	a = st.att
	id := promptID(to, hub.SubmissionID(ctx))
	b.notePrompt(id, msg)
	cctx, cancel := context.WithTimeout(hub.WithSubmissionID(ctx, id), callTimeout)
	defer cancel()
	res, err := b.svc.Prompt(cctx, user, a.backendID, a.sessionID, text, files...)
	switch {
	case err != nil:
		b.reportErr(ctx, to, user, "prompt", err)
	case res.Held:
		b.sendText(ctx, to, msgHeld)
	}
	if err == nil && res.Notice != "" {
		b.sendText(ctx, to, res.Notice)
	}
	if err == nil && res.Output != "" {
		b.sendText(ctx, to, res.Output)
	}
}

func (b *Bot) stop(ctx context.Context, to dest, user string) {
	st := b.chat(to)
	if st.att == nil || st.user != user {
		b.sendText(ctx, to, "No session is attached.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.svc.Interrupt(cctx, user, st.att.backendID, st.att.sessionID); err != nil {
		b.reportErr(ctx, to, user, "interrupt", err)
		return
	}
	b.sendText(ctx, to, "Stop requested.")
}

func (b *Bot) pump(a *attachment) {
	defer b.wg.Done()
	defer close(a.done)
	ctx := a.ctx
	interval := b.cfg.EditInterval
	if a.to.thread != 0 {
		interval = max(interval, groupEditInterval)
	}
	r := newRenderer(b.api, a.to, interval, b.logger.With("chat", a.to.chat, "thread", a.to.thread, "session", a.sessionID),
		func(p hub.PromptInfo, body string) (string, func(int64)) {
			return b.addApproval(a, p, body)
		})
	r.now = b.now
	r.statusTick = b.tick
	r.budget = b.budget
	r.drafts = b.cfg.Drafts && a.to.chat > 0
	r.userMsg = b.sentMessage
	if b.cfg.Steps != "" {
		r.steps = b.cfg.Steps
	}
	r.settle = b.settleApproval
	r.expire = b.expireApproval
	r.shown = func(n int) { a.prompts.Store(int64(n)) }
	r.gone = func() { b.topicGone(a.to) }
	if a.to.thread != 0 {
		r.deleted = func() { b.markDeleted(a.to) }
	}
	r.stepsSnap = func(msgID int64, html string) { b.setSteps(msgKey{a.to.chat, msgID}, html) }
	r.source = func() string { return b.source(hub.SessionRef{BackendID: a.backendID, SessionID: a.sessionID}) }
	if pr, ok := b.svc.(hub.Prompter); ok {
		r.pending = func(ctx context.Context, id string) bool {
			cctx, cancel := context.WithTimeout(ctx, callTimeout)
			defer cancel()
			_, err := pr.PendingPrompt(cctx, a.user, a.backendID, a.sessionID, id)
			return err == nil
		}
	}
	if _, ok := b.notices.(answerFinder); ok {
		r.recorded = func(ctx context.Context, id string) (hub.ApprovalOutcome, bool) {
			return b.recordedAnswer(ctx, a.backendID, id)
		}
	}
	if m, ok := b.svc.(hub.Media); ok {
		r.media = func(ctx context.Context, p string) (*hub.MediaFile, error) {
			return m.Media(ctx, a.user, a.backendID, a.sessionID, p)
		}
	}
	r.pageURL = func(ref string) string {
		return b.public + "/#/page/" + url.PathEscape(a.backendID) + "/" + url.PathEscape(a.sessionID) + "/" + url.PathEscape(ref)
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
				b.logger.Info("Session stream ended", "chat", a.to.chat, "thread", a.to.thread, "session", a.sessionID)
				return
			}
			a.lastEvent.Store(b.now().UnixNano())
			a.status.track(ev)
			r.handle(ctx, ev)
			if ev.Seq > a.lastSeq.Load() {
				a.lastSeq.Store(ev.Seq)
			}
		case <-timer.C:
			r.flushDue(ctx)
			r.expireDue(ctx)
		}
		if d, ok := r.nextFlush(); ok {
			timer.Reset(d)
		}
	}
}

// addApproval keeps a prompt that an attached chat shows. Its bind
// records the message, which a reply to answers a question.
func (b *Bot) addApproval(a *attachment, prompt hub.PromptInfo, body string) (string, func(int64)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw := make([]byte, 9)
	_, _ = rand.Read(raw)
	key := base64.RawURLEncoding.EncodeToString(raw)
	p := &pendingApproval{to: a.to, backendID: a.backendID, sessionID: a.sessionID, prompt: prompt, body: body,
		bound: make(chan struct{})}
	b.approvals[key] = p
	return key, func(id int64) {
		b.mu.Lock()
		defer b.mu.Unlock()
		p.messageID = id
		select {
		case <-p.bound:
		default:
			close(p.bound)
		}
		if id != 0 && prompt.Kind == hub.PromptClarify {
			b.addReplyLocked(msgKey{a.to.chat, id}, replyTarget{prefix: cbApproval, key: key, question: id})
		}
	}
}

func (b *Bot) handleCallback(ctx context.Context, q *CallbackQuery) {
	if q.Message == nil || !b.chatAllowed(q.Message.Chat) && b.forum(q.Message.Chat.ID) == nil {
		b.answer(ctx, q.ID, "")
		return
	}
	to := dest{chat: q.Message.Chat.ID}
	if q.Message.IsTopicMessage {
		to.thread = q.Message.MessageThreadID
	}
	user, linked, err := b.lookupUser(ctx, q.From.ID)
	if err != nil {
		b.answer(ctx, q.ID, errGeneric)
		return
	}
	if !linked {
		b.answer(ctx, q.ID, "Link your Telegram account first: send me a direct message.")
		return
	}
	if f := b.forum(to.chat); f != nil && f.user != user {
		b.answer(ctx, q.ID, msgNotYours)
		return
	}
	parts := strings.Split(q.Data, ":")
	switch {
	case len(parts) == 3 && (parts[0] == cbApproval || parts[0] == cbNotice):
		t, refusal := b.attachedTarget(ctx, to, q.Message.MessageID, parts[1])
		if parts[0] == cbNotice {
			t, refusal = b.noticeTarget(ctx, to, q.Message.MessageID, user, parts[1])
		}
		if refusal != "" {
			b.answer(ctx, q.ID, refusal)
			return
		}
		b.pressPrompt(ctx, q, user, t, parts[2])
	case modelButton(parts):
		b.pressModel(ctx, q, to, user, parts)
	case len(parts) == 3 && parts[0] == cbSavedPrompt:
		b.pressSavedPrompt(ctx, q, to, user, parts)
	case len(parts) == 1 && parts[0] == cbSteps:
		b.sendSteps(ctx, q, to)
	case len(parts) == 2 && parts[0] == cbRelease && b.computers != nil:
		b.confirmRelease(ctx, q, to, user, parts[1])
	case len(parts) == 3 && parts[0] == cbAttach:
		st := b.chat(to)
		gen, err1 := strconv.Atoi(parts[1])
		n, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || gen != st.listGen || n < 1 || n > len(st.list) {
			b.answer(ctx, q.ID, "That list is out of date. Send /sessions again.")
			return
		}
		b.answer(ctx, q.ID, "")
		b.attachListed(ctx, to, user, st.list[n-1])
	default:
		b.answer(ctx, q.ID, "")
	}
}

// settleApproval forgets the prompt of an attached chat that got its
// answer, and returns it, or false when it was answered here.
func (b *Bot) settleApproval(key string) (pendingApproval, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.approvals[key]
	if !ok {
		return pendingApproval{}, false
	}
	delete(b.approvals, key)
	delete(b.picks, cbApproval+":"+key)
	b.dropRepliesLocked(cbApproval, key)
	return *p, true
}

// expireApproval forgets the prompt of an attached chat that Hermes no
// longer waits for, and returns it, or false when it got its answer.
func (b *Bot) expireApproval(key string) (pendingApproval, bool) {
	return b.settleApproval(key)
}

func (b *Bot) reportErr(ctx context.Context, to dest, user, action string, err error) {
	if b.computerErr(ctx, to, err) {
		b.logger.Info("Telegram computer request refused", "user", user, "action", action, "error", err)
		return
	}
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Hub denied Telegram request", "user", user, "action", action, "error", err)
		b.sendText(ctx, to, "You do not have access to that backend.")
	case errors.Is(err, hub.ErrUnknownBackend):
		b.sendText(ctx, to, "Unknown backend. Send /backends to list yours.")
	case errors.Is(err, hub.ErrShuttingDown):
		b.sendText(ctx, to, "The hub is restarting. Send your message again in a moment.")
	case errors.Is(err, hub.ErrAttachment):
		b.sendText(ctx, to, hub.AttachmentReason(err)+".")
	case errors.Is(err, hub.ErrHoldFull):
		b.sendText(ctx, to, "Too many messages are waiting for the agent to come back. Send this one again later.")
	default:
		b.logger.Error("Telegram request failed", "user", user, "action", action, "error", err)
		b.sendText(ctx, to, errGeneric)
	}
}

func (b *Bot) answer(ctx context.Context, id, text string) {
	if err := b.api.AnswerCallback(ctx, id, text); err != nil {
		b.logger.Warn("Failed to answer callback", "error", err)
	}
}

func (b *Bot) sendText(ctx context.Context, to dest, text string) {
	if _, err := b.api.SendText(ctx, to.chat, to.thread, text); err != nil {
		b.logger.Warn("Failed to send message", "chat", to.chat, "error", err)
	}
}

func (b *Bot) sendHTML(ctx context.Context, to dest, text string, kb *InlineKeyboard) {
	sendHTML(ctx, b.api, b.logger, to, text, kb)
}

func (b *Bot) editHTML(ctx context.Context, chatID, messageID int64, text string) {
	if err := b.api.EditHTML(ctx, chatID, messageID, text, nil); err != nil {
		b.logger.Warn("Failed to edit message", "chat", chatID, "message", messageID, "error", err)
	}
}

// sendHTML sends text as HTML and returns the message ID, or 0 on failure.
// When Telegram rejects the markup it resends the text without tags.
func sendHTML(ctx context.Context, api *Client, logger *slog.Logger, to dest, text string, kb *InlineKeyboard) int64 {
	id, _ := sendHTMLErr(ctx, api, logger, to, text, kb)
	return id
}

// sendHTMLErr is sendHTML that also returns the error.
func sendHTMLErr(ctx context.Context, api *Client, logger *slog.Logger, to dest, text string, kb *InlineKeyboard) (int64, error) {
	id, err := api.SendHTML(ctx, to.chat, to.thread, text, kb)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.BadEntities() {
		logger.Warn("Telegram rejected HTML, sending plain text", "chat", to.chat, "error", err)
		plain := html.UnescapeString(tagRe.ReplaceAllString(text, ""))
		var m Message
		err = api.call(ctx, "sendMessage", sendMessageParams{
			ChatID: to.chat, MessageThreadID: to.thread, Text: plain, ReplyMarkup: markup(kb), LinkPreview: &linkPreview{IsDisabled: true},
		}, &m)
		id = m.MessageID
	}
	if err != nil {
		logger.Warn("Failed to send message", "chat", to.chat, "error", err)
		return 0, err
	}
	return id, nil
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

// setSteps keeps the steps so far of the turn whose status line is k; ""
// forgets them when the turn ends.
func (b *Bot) setSteps(k msgKey, html string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if html == "" {
		delete(b.steps, k)
		return
	}
	b.steps[k] = html
}

// sendSteps answers a Steps button with a new message of the steps so far.
// That message is never edited, so a quote opened in it stays open.
func (b *Bot) sendSteps(ctx context.Context, q *CallbackQuery, to dest) {
	b.mu.Lock()
	html, ok := b.steps[msgKey{to.chat, q.Message.MessageID}]
	b.mu.Unlock()
	if !ok {
		b.answer(ctx, q.ID, "This turn is done. Its steps are in the summary.")
		return
	}
	b.answer(ctx, q.ID, "")
	if _, err := sendHTMLErr(ctx, b.api, b.logger, to, html, nil); err != nil {
		b.logger.Warn("Failed to send the steps", "chat", to.chat, "error", err)
	}
}
