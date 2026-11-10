package telegram_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

const (
	testToken    = "123:abc"
	mSend        = "sendMessage"
	mEdit        = "editMessageText"
	mAnswer      = "answerCallbackQuery"
	kText        = "text"
	kStatus      = "status"
	kDesc        = "description"
	kResult      = "result"
	userAlice    = "alice"
	userBob      = "bob"
	beCluster    = "cluster"
	beLaptop     = "laptop"
	beHers       = "hers"
	toolTerminal = "terminal"
)

type tgCall struct {
	Method   string
	Params   map[string]any
	At       time.Time
	ResultID int64
}

func (c tgCall) text() string {
	s, _ := c.Params[kText].(string)
	return s
}

func (c tgCall) messageID() int64 {
	f, _ := c.Params["message_id"].(float64)
	return int64(f)
}

func (c tgCall) buttons() []map[string]any {
	markup, _ := c.Params["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	var out []map[string]any
	for _, row := range rows {
		cells, _ := row.([]any)
		for _, cell := range cells {
			if m, ok := cell.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// fakeTelegram is an httptest Bot API. It hands out queued updates and
// records every call.
type fakeTelegram struct {
	t       *testing.T
	srv     *httptest.Server
	updates chan telegram.Update

	mu         sync.Mutex
	calls      []tgCall
	nextID     int64
	nextUpdate int64
	fail429    map[string]int
	rejectHTML int
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{t: t, updates: make(chan telegram.Update, 64), fail429: map[string]int{}, nextID: 100}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTelegram) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode: %v", err)
	}
}

func (f *fakeTelegram) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	params := map[string]any{}
	if err := json.Unmarshal(body, &params); err != nil {
		f.t.Errorf("bad body for %s: %v", method, err)
	}
	if method == "getUpdates" {
		f.serveUpdates(w, r)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, tgCall{Method: method, Params: params, At: time.Now()})
	idx := len(f.calls) - 1
	if f.fail429[method] > 0 {
		f.fail429[method]--
		f.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		f.reply(w, map[string]any{"ok": false, "error_code": 429,
			kDesc: "Too Many Requests: retry after 1", "parameters": map[string]any{"retry_after": 1}})
		return
	}
	if method == mSend && params["parse_mode"] == "HTML" && f.rejectHTML > 0 {
		f.rejectHTML--
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		f.reply(w, map[string]any{"ok": false, "error_code": 400,
			kDesc: "Bad Request: can't parse entities: unexpected end tag"})
		return
	}
	f.nextID++
	id := f.nextID
	if method == mSend {
		f.calls[idx].ResultID = id
	}
	f.mu.Unlock()
	switch method {
	case mSend:
		f.reply(w, map[string]any{"ok": true, kResult: map[string]any{
			"message_id": id, "chat": map[string]any{"id": params["chat_id"], "type": "private"}, kText: params[kText]}})
	default:
		f.reply(w, map[string]any{"ok": true, kResult: true})
	}
}

func (f *fakeTelegram) serveUpdates(w http.ResponseWriter, r *http.Request) {
	var out []telegram.Update
	select {
	case u := <-f.updates:
		out = append(out, u)
	case <-time.After(50 * time.Millisecond):
	case <-r.Context().Done():
		return
	}
	f.reply(w, map[string]any{"ok": true, kResult: out})
}

func (f *fakeTelegram) push(u telegram.Update) {
	f.mu.Lock()
	f.nextUpdate++
	u.UpdateID = f.nextUpdate
	f.mu.Unlock()
	f.updates <- u
}

func (f *fakeTelegram) say(chat telegram.Chat, from int64, text string) {
	f.push(telegram.Update{Message: &telegram.Message{
		MessageID: 1, From: &telegram.User{ID: from, FirstName: "u"}, Chat: chat, Text: text,
	}})
}

func (f *fakeTelegram) press(chat telegram.Chat, from, messageID int64, data string) {
	f.push(telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		ID: fmt.Sprintf("cb%d", time.Now().UnixNano()), From: telegram.User{ID: from},
		Message: &telegram.Message{MessageID: messageID, Chat: chat}, Data: data,
	}})
}

func (f *fakeTelegram) snapshot() []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeTelegram) find(method, contains string) []tgCall {
	var out []tgCall
	for _, c := range f.snapshot() {
		if c.Method == method && strings.Contains(c.text(), contains) {
			out = append(out, c)
		}
	}
	return out
}

// waitN polls until n calls match and returns them.
func (f *fakeTelegram) waitN(method, contains string, n int) []tgCall {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.find(method, contains); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("wanted %d %s calls containing %q, got %d", n, method, contains, len(f.find(method, contains)))
	return nil
}

// wait polls until one call matches and returns it.
func (f *fakeTelegram) wait(method, contains string) tgCall {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.find(method, contains); len(got) > 0 {
			return got[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("no %s call containing %q; calls: %+v", method, contains, f.snapshot())
	return tgCall{}
}

type promptCall struct{ User, BackendID, SessionID, Text string }

type approvalCall struct {
	User, BackendID, SessionID, ApprovalID string
	Allow                                  bool
}

type stream struct {
	ch   chan hub.Event
	once sync.Once
}

// fakeHub is a hub.Service with per-user backends that enforces the same
// authorization the real hub does.
type fakeHub struct {
	mu          sync.Mutex
	backends    map[string][]hub.Backend
	sessions    []hub.Session
	streams     map[string]*stream
	attaches    []string
	prompts     []promptCall
	interrupts  []string
	approvals   []approvalCall
	approvalErr error
	seq         int64
	// down maps backend ids to their last connection error.
	down map[string]string
}

func newFakeHub() *fakeHub {
	return &fakeHub{
		backends: map[string][]hub.Backend{
			userAlice: {{ID: beLaptop, Name: "Laptop"}, {ID: beCluster, Name: "Cluster"}},
			userBob:   {{ID: beHers, Name: "Hers"}},
		},
		streams: map[string]*stream{},
	}
}

func (h *fakeHub) allowed(user, backendID string) error {
	for _, b := range h.backends[user] {
		if b.ID == backendID {
			return nil
		}
	}
	return hub.ErrForbidden
}

func (h *fakeHub) Backends(_ context.Context, user string) ([]hub.Backend, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.backends[user]), nil
}

func (h *fakeHub) Sessions(_ context.Context, user string) ([]hub.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hub.Session
	for _, s := range h.sessions {
		if h.allowed(user, s.BackendID) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func (h *fakeHub) NewSession(_ context.Context, user, backendID string) (hub.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return hub.Session{}, err
	}
	s := hub.Session{ID: fmt.Sprintf("s%d", len(h.sessions)+1), BackendID: backendID, UpdatedAt: time.Now()}
	h.sessions = append(h.sessions, s)
	return s, nil
}

func (h *fakeHub) Attach(_ context.Context, user, backendID, sessionID string, _ int64) (<-chan hub.Event, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return nil, nil, err
	}
	st := &stream{ch: make(chan hub.Event, 256)}
	h.streams[sessionID] = st
	h.attaches = append(h.attaches, sessionID)
	return st.ch, func() { st.once.Do(func() { close(st.ch) }) }, nil
}

func (h *fakeHub) Prompt(_ context.Context, user, backendID, sessionID, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return err
	}
	h.prompts = append(h.prompts, promptCall{user, backendID, sessionID, text})
	return nil
}

func (h *fakeHub) Interrupt(_ context.Context, user, backendID, sessionID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return err
	}
	h.interrupts = append(h.interrupts, sessionID)
	return nil
}

func (h *fakeHub) RespondApproval(_ context.Context, user, backendID, sessionID, approvalID string, allow bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return err
	}
	if h.approvalErr != nil {
		return h.approvalErr
	}
	h.approvals = append(h.approvals, approvalCall{user, backendID, sessionID, approvalID, allow})
	return nil
}

func (h *fakeHub) emit(t *testing.T, sessionID, kind string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	st := h.streams[sessionID]
	h.seq++
	ev := hub.Event{Seq: h.seq, Kind: kind, Data: raw}
	h.mu.Unlock()
	if st == nil {
		t.Fatalf("no stream for %s", sessionID)
	}
	st.ch <- ev
}

func (h *fakeHub) promptsCopy() []promptCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.prompts)
}

func (h *fakeHub) waitPrompt(t *testing.T, n int) []promptCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := h.promptsCopy(); len(p) >= n {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wanted %d prompts, got %+v", n, h.promptsCopy())
	return nil
}

type fakeLinks struct {
	mu    sync.Mutex
	users map[int64]string
}

func (l *fakeLinks) UserForTelegram(_ context.Context, id int64) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.users[id]
	return u, ok, nil
}

func (l *fakeLinks) LinkTelegram(_ context.Context, id int64, user string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.users[id] = user
	return nil
}

type fakeTokens struct {
	mu      sync.Mutex
	created []int64
}

func (f *fakeTokens) CreateLinkToken(_ context.Context, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, id)
	return fmt.Sprintf("tok-%d&x", id), nil
}

func (f *fakeTokens) ConsumeLinkToken(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}

const (
	aliceTG = 1
	bobTG   = 2
)

type harness struct {
	tg     *fakeTelegram
	hub    *fakeHub
	links  *fakeLinks
	tokens *fakeTokens
}

func dm(id int64) telegram.Chat { return telegram.Chat{ID: id, Type: "private"} }

func testConfig(f *fakeTelegram) telegram.Config {
	return telegram.Config{
		Enabled:         true,
		Token:           testToken,
		APIURL:          f.srv.URL,
		DefaultBackends: []string{"alice=cluster"},
		AllowedGroups:   []int64{-500},
		PollTimeout:     time.Second,
		EditInterval:    300 * time.Millisecond,
		MaxRetries:      3,
	}
}

func startBot(t *testing.T, mutate func(*telegram.Config)) *harness {
	t.Helper()
	h := &harness{
		tg:     newFakeTelegram(t),
		hub:    newFakeHub(),
		links:  &fakeLinks{users: map[int64]string{aliceTG: userAlice, bobTG: userBob}},
		tokens: &fakeTokens{},
	}
	cfg := testConfig(h.tg)
	if mutate != nil {
		mutate(&cfg)
	}
	bot, err := telegram.New(cfg, telegram.Options{
		PublicURL: "https://hub.example/",
		Service:   h.hub, Links: h.links, Tokens: h.tokens,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})
	return h
}

// startTurn sends a prompt as Alice and returns the session it went to.
func (h *harness) startTurn(t *testing.T) string {
	t.Helper()
	h.tg.say(dm(aliceTG), aliceTG, "hello")
	return h.hub.waitPrompt(t, 1)[0].SessionID
}

func (h *fakeHub) CancelInput(context.Context, string, string, string, string) error {
	return hub.ErrUnknownInput
}

func (h *fakeHub) DismissLost(context.Context, string, string, string, string) error {
	return hub.ErrUnknownInput
}

func (h *fakeHub) RenameSession(context.Context, string, string, string, string) error { return nil }

func (h *fakeHub) ArchiveSession(context.Context, string, string, string, bool) error { return nil }

func (h *fakeHub) DeleteSession(context.Context, string, string, string) error { return nil }

func (h *fakeHub) Models(context.Context, string, string, string) (hub.Models, error) {
	return hub.Models{}, nil
}

func (h *fakeHub) SetModel(context.Context, string, string, string, string, string, string) (hub.ModelChange, error) {
	return hub.ModelChange{Status: hub.ModelChanged}, nil
}

func (h *fakeHub) SetReasoning(context.Context, string, string, string, string) error { return nil }

func (h *fakeHub) BackendStatus(_ context.Context, user string) (map[string]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for _, b := range h.backends[user] {
		out[b.ID] = h.down[b.ID]
	}
	return out, nil
}
