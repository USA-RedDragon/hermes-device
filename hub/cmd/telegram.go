package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/notify"
	"github.com/USA-RedDragon/talaria/hub/internal/secretbox"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/telegram"
)

// telegramSecret is the name of the bot token set in Settings, Telegram.
const telegramSecret = "telegram_token"

// botWatch is how often a replica looks for a token changed on another one.
const botWatch = 15 * time.Second

// botManager runs the Telegram bot with the token the install sets
// (telegram.token or telegram.token_file), else with the one an admin set in
// Settings, Telegram, kept sealed in the hub's database. A token set in
// Settings starts the bot at once, without a restart; every replica picks
// it up within botWatch. It is the notifier's Telegram, forwarding to the
// running bot.
type botManager struct {
	base   config.Telegram
	opts   telegram.Options
	st     *store.Store
	box    *secretbox.Box
	client *http.Client

	mu      sync.RWMutex
	bot     *telegram.Bot
	token   string
	cancel  context.CancelFunc
	done    chan struct{}
	lastErr string
	me      string
}

var _ notify.Telegram = (*botManager)(nil)

func newBotManager(cfg config.Telegram, opts telegram.Options, st *store.Store, box *secretbox.Box) *botManager {
	return &botManager{base: cfg, opts: opts, st: st, box: box, client: &http.Client{Timeout: 15 * time.Second}}
}

// fromInstall reports a token the install sets: Settings shows it read-only.
func (m *botManager) fromInstall() bool { return m.base.Token != "" }

// wanted is the token the bot should run with, "" for none.
func (m *botManager) wanted(ctx context.Context) (string, error) {
	if m.fromInstall() {
		if !m.base.Enabled {
			return "", nil
		}
		return m.base.Token, nil
	}
	if m.box == nil {
		return "", nil
	}
	tok, _, err := m.box.Get(ctx, telegramSecret)
	if errors.Is(err, secretbox.ErrSealedElsewhere) {
		return "", nil
	}
	return tok, err
}

// run starts the bot and keeps it on the wanted token until ctx ends.
func (m *botManager) run(ctx context.Context) {
	m.sync(ctx)
	t := time.NewTicker(botWatch)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stop()
			return
		case <-t.C:
			m.sync(ctx)
		}
	}
}

// sync starts, stops or restarts the bot to match the wanted token.
func (m *botManager) sync(ctx context.Context) {
	tok, err := m.wanted(ctx)
	if err != nil {
		slog.Warn("Could not read the Telegram bot token", "error", err.Error())
		return
	}
	m.mu.RLock()
	same := tok == m.token
	m.mu.RUnlock()
	if same {
		return
	}
	m.stop()
	if tok == "" {
		return
	}
	cfg := m.base
	cfg.Enabled, cfg.Token = true, tok
	bot, err := telegram.New(telegram.Config(cfg), m.opts)
	if err != nil {
		m.mu.Lock()
		m.lastErr, m.token = err.Error(), tok
		m.mu.Unlock()
		slog.Error("Could not start the Telegram bot", "error", err.Error())
		return
	}
	botCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	m.mu.Lock()
	m.bot, m.token, m.cancel, m.done, m.lastErr, m.me = bot, tok, cancel, done, "", ""
	m.mu.Unlock()
	go runBot(botCtx, m.st, bot, cfg, done)
	slog.Info("Telegram bot started", "source", map[bool]string{true: "install", false: "settings"}[m.fromInstall()])
}

// stop stops the running bot and waits for it, up to 20 seconds.
func (m *botManager) stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.bot, m.token, m.cancel, m.done = nil, "", nil, nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		slog.Warn("Telegram bot did not stop in time")
	}
}

// current is the running bot, nil when none.
func (m *botManager) current() *telegram.Bot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bot
}

func (m *botManager) SendNotice(ctx context.Context, chatID int64, n notify.Message) (int64, error) {
	b := m.current()
	if b == nil {
		return 0, errors.New("the Telegram bot is not running")
	}
	return b.SendNotice(ctx, chatID, n)
}

func (m *botManager) Settled(ctx context.Context, rec hub.NoticeRecord, cancelled bool, msgs []hub.NoticeMessage) {
	if b := m.current(); b != nil {
		b.Settled(ctx, rec, cancelled, msgs)
	}
}

func (m *botManager) Outcome(ctx context.Context, rec hub.NoticeRecord, o hub.ApprovalOutcome, msgs []hub.NoticeMessage) {
	if b := m.current(); b != nil {
		b.Outcome(ctx, rec, o, msgs)
	}
}

// ---- hub.TelegramAdmin ----

// TelegramStatus implements hub.TelegramAdmin.
func (m *botManager) TelegramStatus(ctx context.Context) hub.TelegramStatus {
	st := hub.TelegramStatus{Install: m.fromInstall(), Steps: m.Steps()}
	m.mu.RLock()
	st.Running, st.Error, st.Bot = m.bot != nil, m.lastErr, m.me
	tok := m.token
	m.mu.RUnlock()
	if !st.Install && m.box != nil {
		_, ok, err := m.box.Get(ctx, telegramSecret)
		st.Saved = ok
		if errors.Is(err, secretbox.ErrSealedElsewhere) {
			st.Error = "The saved token was made with another hub key and cannot be read. Enter it again."
		}
	}
	if st.Running && st.Bot == "" && tok != "" {
		if name, err := m.TestTelegram(ctx, tok); err == nil {
			st.Bot = name
			m.mu.Lock()
			if m.token == tok {
				m.me = name
			}
			m.mu.Unlock()
		}
	}
	return st
}

// Steps is the bot's steps setting while a bot runs, else "".
func (m *botManager) Steps() string {
	if m.current() == nil && (!m.fromInstall() || !m.base.Enabled) {
		return ""
	}
	if m.base.Steps == "" {
		return telegram.StepsCollapsed
	}
	return m.base.Steps
}

// errBadToken is Telegram's refusal of a token, in plain words.
var errBadToken = &hub.TelegramError{Status: http.StatusBadRequest, Message: "Telegram says this token is not valid. Copy it again from @BotFather, or make a new one with /token."}

// TestTelegram implements hub.TelegramAdmin: getMe with the token.
func (m *botManager) TestTelegram(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if !validToken(token) {
		return "", errBadToken
	}
	c := telegram.NewClient(m.base.APIURL, token, m.client, 1, slog.Default())
	me, err := c.GetMe(ctx)
	if err != nil {
		var ae *telegram.APIError
		if errors.As(err, &ae) && (ae.Code == http.StatusUnauthorized || ae.Code == http.StatusNotFound) {
			return "", errBadToken
		}
		return "", &hub.TelegramError{Status: http.StatusBadGateway, Message: "Could not reach Telegram. Check the server's internet connection and try again."}
	}
	return "@" + me.Username, nil
}

func validToken(t string) bool {
	id, secret, ok := strings.Cut(t, ":")
	if !ok || len(id) < 3 || len(secret) < 20 || len(t) > 200 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return !strings.ContainsAny(t, " /\n\t")
}

// SaveTelegram implements hub.TelegramAdmin: test, keep, start.
func (m *botManager) SaveTelegram(ctx context.Context, token string) (string, error) {
	if m.fromInstall() {
		return "", &hub.TelegramError{Status: http.StatusConflict, Message: "The token is set in this server's install settings, so it cannot be changed here."}
	}
	if m.box == nil {
		return "", &hub.TelegramError{Status: http.StatusConflict, Message: "This hub cannot keep secrets."}
	}
	token = strings.TrimSpace(token)
	name, err := m.TestTelegram(ctx, token)
	if err != nil {
		return "", err
	}
	if err := m.box.Put(ctx, telegramSecret, token); err != nil {
		return "", err
	}
	m.sync(ctx)
	m.mu.Lock()
	m.me = name
	m.mu.Unlock()
	return name, nil
}

// ClearTelegram implements hub.TelegramAdmin: forget the token, stop.
func (m *botManager) ClearTelegram(ctx context.Context) error {
	if m.fromInstall() {
		return &hub.TelegramError{Status: http.StatusConflict, Message: "The token is set in this server's install settings, so it cannot be changed here."}
	}
	if m.box == nil {
		return nil
	}
	if err := m.box.Delete(ctx, telegramSecret); err != nil {
		return err
	}
	m.sync(ctx)
	return nil
}
