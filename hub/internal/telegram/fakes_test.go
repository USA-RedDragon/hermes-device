package telegram_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strconv"
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
	mPhoto       = "sendPhoto"
	mDocument    = "sendDocument"
	mTopic       = "createForumTopic"
	kText        = "text"
	kStatus      = "status"
	kDesc        = "description"
	kErrorCode   = "error_code"
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
	// rejectPhoto makes sendPhoto fail as for a photo of bad dimensions.
	rejectPhoto bool
	offsets     []int64
	// files are the files getFile and downloads serve, by file id.
	files map[string][]byte
	// sizes overrides the file_size getFile reports.
	sizes map[string]int64
	// member is what getChatMember says of the bot.
	member map[string]any
	// goneThreads are topics whose messages Telegram refuses.
	goneThreads map[int64]bool
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{t: t, updates: make(chan telegram.Update, 64), fail429: map[string]int{}, nextID: 100,
		files: map[string][]byte{}, sizes: map[string]int64{}, goneThreads: map[int64]bool{},
		member: map[string]any{kStatus: "administrator", "can_manage_topics": true}}
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
	if id, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+testToken+"/docs/"); ok {
		f.mu.Lock()
		data, found := f.files[id]
		f.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	params := map[string]any{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		params = f.form(r)
	} else if body, _ := io.ReadAll(r.Body); json.Unmarshal(body, &params) != nil {
		f.t.Errorf("bad body for %s", method)
	}
	if method == "getUpdates" {
		off, _ := params["offset"].(float64)
		f.mu.Lock()
		f.offsets = append(f.offsets, int64(off))
		f.mu.Unlock()
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
		f.reply(w, map[string]any{"ok": false, kErrorCode: 429,
			kDesc: "Too Many Requests: retry after 1", "parameters": map[string]any{"retry_after": 1}})
		return
	}
	if method == mPhoto && f.rejectPhoto {
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		f.reply(w, map[string]any{"ok": false, kErrorCode: 400, kDesc: "Bad Request: PHOTO_INVALID_DIMENSIONS"})
		return
	}
	if method == mSend && params["parse_mode"] == "HTML" && f.rejectHTML > 0 {
		f.rejectHTML--
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		f.reply(w, map[string]any{"ok": false, kErrorCode: 400,
			kDesc: "Bad Request: can't parse entities: unexpected end tag"})
		return
	}
	if thread, _ := params["message_thread_id"].(float64); method == mSend && f.goneThreads[int64(thread)] {
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		f.reply(w, map[string]any{"ok": false, kErrorCode: 400, kDesc: "Bad Request: message thread not found"})
		return
	}
	f.nextID++
	id := f.nextID
	if method == mSend || method == mTopic {
		f.calls[idx].ResultID = id
	}
	member := f.member
	f.mu.Unlock()
	switch method {
	case mTopic:
		f.reply(w, map[string]any{"ok": true, kResult: map[string]any{"message_thread_id": id, kName: params[kName]}})
	case "getChatMember":
		f.reply(w, map[string]any{"ok": true, kResult: member})
	case mSend:
		f.reply(w, map[string]any{"ok": true, kResult: map[string]any{
			"message_id": id, "chat": map[string]any{"id": params["chat_id"], "type": "private"}, kText: params[kText]}})
	case "getFile":
		fid, _ := params["file_id"].(string)
		f.mu.Lock()
		data, found := f.files[fid]
		size, sized := f.sizes[fid]
		f.mu.Unlock()
		if !sized {
			size = int64(len(data))
		}
		if !found {
			f.reply(w, map[string]any{"ok": false, kErrorCode: 400, kDesc: "Bad Request: invalid file_id"})
			return
		}
		f.reply(w, map[string]any{"ok": true, kResult: map[string]any{"file_id": fid, "file_size": size,
			"file_path": "docs/" + fid}})
	default:
		f.reply(w, map[string]any{"ok": true, kResult: true})
	}
}

// form reads a file upload: its chat_id, and the file's field, name and
// bytes as "field", "name" and "data".
func (f *fakeTelegram) form(r *http.Request) map[string]any {
	params := map[string]any{}
	mr, err := r.MultipartReader()
	if err != nil {
		f.t.Errorf("bad upload: %v", err)
		return params
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return params
		}
		data, _ := io.ReadAll(io.LimitReader(part, 64<<20))
		if part.FileName() == "" {
			if id, err := strconv.ParseFloat(string(data), 64); part.FormName() == "chat_id" && err == nil {
				params["chat_id"] = id
			}
			continue
		}
		params["field"], params["name"], params["data"] = part.FormName(), part.FileName(), string(data)
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

// replyTo sends text as a reply to the message replyTo.
func (f *fakeTelegram) replyTo(chat telegram.Chat, from, replyTo int64, text string) {
	f.push(telegram.Update{Message: &telegram.Message{
		MessageID: 1, From: &telegram.User{ID: from, FirstName: "u"}, Chat: chat, Text: text,
		ReplyToMessage: &telegram.Message{MessageID: replyTo, Chat: chat},
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

type promptCall struct {
	User, BackendID, SessionID, Text, SubmissionID string
	Files                                          []hub.Attachment
}

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
	answers     []answerCall
	approvalErr error
	seq         int64
	// down maps backend ids to their last connection error.
	down map[string]string
	// hold makes Prompt answer that it held the prompt.
	hold bool
	// notice is the Notice Prompt answers.
	notice string
	// media are the files Media opens, by path, and mediaCalls its calls.
	media      map[string]fakeFile
	mediaCalls []string
	// pending are the prompts PendingPrompt finds.
	pending map[string]bool
}

type fakeFile struct {
	mime string
	data string
	err  error
}

// Media opens a file of h.media for a user who may use the backend.
func (h *fakeHub) Media(_ context.Context, user, backendID, sessionID, p string) (*hub.MediaFile, error) {
	if err := h.allowed(user, backendID); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mediaCalls = append(h.mediaCalls, user+" "+backendID+" "+sessionID+" "+p)
	f, ok := h.media[p]
	if !ok {
		return nil, hub.ErrNoMedia
	}
	if f.err != nil {
		return nil, f.err
	}
	return &hub.MediaFile{Name: path.Base(p), MIME: f.mime, Size: int64(len(f.data)), Body: io.NopCloser(strings.NewReader(f.data))}, nil
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

func (h *fakeHub) Prompt(ctx context.Context, user, backendID, sessionID, text string, files ...hub.Attachment) (hub.PromptResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return hub.PromptResult{}, err
	}
	if err := hub.CheckAttachments(files); err != nil {
		return hub.PromptResult{}, err
	}
	h.prompts = append(h.prompts, promptCall{user, backendID, sessionID, text, hub.SubmissionID(ctx), files})
	return hub.PromptResult{ID: fmt.Sprintf("p%d", len(h.prompts)), Held: h.hold, Notice: h.notice}, nil
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

type answerCall struct {
	User, BackendID, SessionID, PromptID string
	Answer                               hub.Answer
}

// AnswerPrompt records answers; an approval choice also counts as an
// approval, allowed when it is once.
func (h *fakeHub) AnswerPrompt(_ context.Context, user, backendID, sessionID, promptID string, a hub.Answer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.allowed(user, backendID); err != nil {
		return err
	}
	if h.approvalErr != nil {
		return h.approvalErr
	}
	h.answers = append(h.answers, answerCall{user, backendID, sessionID, promptID, a})
	if a.Choice != "" {
		h.approvals = append(h.approvals, approvalCall{user, backendID, sessionID, promptID, a.Choice == hub.ChoiceOnce})
	}
	return nil
}

func (h *fakeHub) PendingPrompt(_ context.Context, _, _, _, promptID string) (hub.PromptInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending[promptID] {
		return hub.PromptInfo{ID: promptID}, nil
	}
	return hub.PromptInfo{}, hub.ErrUnknownApproval
}

func (h *fakeHub) answersCopy() []answerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.answers)
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
	bot    *telegram.Bot
	tg     *fakeTelegram
	hub    *fakeHub
	links  *fakeLinks
	tokens *fakeTokens
	state  *fakeState
}

// fakeState is a hub.TelegramState in memory.
type fakeState struct {
	mu     sync.Mutex
	chats  map[int64]hub.TelegramChat
	offset int64
	groups map[int64]hub.TelegramGroup
	topics map[string]hub.TelegramTopic
}

func newFakeState() *fakeState {
	return &fakeState{chats: map[int64]hub.TelegramChat{}, groups: map[int64]hub.TelegramGroup{}, topics: map[string]hub.TelegramTopic{}}
}

func topicKey(chatID int64, backendID, sessionID string) string {
	return fmt.Sprintf("%d/%s/%s", chatID, backendID, sessionID)
}

func (f *fakeState) LinkTelegramGroup(_ context.Context, g hub.TelegramGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, old := range f.groups {
		if old.User == g.User || id == g.ChatID {
			delete(f.groups, id)
			for k, t := range f.topics {
				if t.ChatID == id {
					delete(f.topics, k)
				}
			}
		}
	}
	f.groups[g.ChatID] = g
	return nil
}

func (f *fakeState) UnlinkTelegramGroup(_ context.Context, chatID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.groups, chatID)
	for k, t := range f.topics {
		if t.ChatID == chatID {
			delete(f.topics, k)
		}
	}
	return nil
}

func (f *fakeState) TelegramGroups(context.Context) ([]hub.TelegramGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]hub.TelegramGroup, 0, len(f.groups))
	for _, g := range f.groups {
		out = append(out, g)
	}
	return out, nil
}

func (f *fakeState) SaveTelegramTopic(_ context.Context, t hub.TelegramTopic) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topics[topicKey(t.ChatID, t.BackendID, t.SessionID)] = t
	return nil
}

func (f *fakeState) TelegramTopics(_ context.Context, chatID int64) ([]hub.TelegramTopic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hub.TelegramTopic
	for _, t := range f.topics {
		if t.ChatID == chatID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeState) topic(chatID int64, backendID, sessionID string) (hub.TelegramTopic, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.topics[topicKey(chatID, backendID, sessionID)]
	return t, ok
}

func (f *fakeState) SaveTelegramChat(_ context.Context, c hub.TelegramChat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats[c.ChatID] = c
	return nil
}

func (f *fakeState) TelegramChats(context.Context) ([]hub.TelegramChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]hub.TelegramChat, 0, len(f.chats))
	for _, c := range f.chats {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeState) TelegramOffset(_ context.Context, botID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if botID != 123 {
		return 0, fmt.Errorf("bot id %d", botID)
	}
	return f.offset, nil
}

func (f *fakeState) SetTelegramOffset(_ context.Context, _, offset int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offset = offset
	return nil
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
		LeaderRetry:     time.Second,
		TopicSync:       50 * time.Millisecond,
		EditInterval:    300 * time.Millisecond,
		MaxRetries:      3,
	}
}

func startBot(t *testing.T, mutate func(*telegram.Config)) *harness {
	t.Helper()
	return startBotState(t, mutate, newFakeState())
}

func startBotState(t *testing.T, mutate func(*telegram.Config), state *fakeState) *harness {
	t.Helper()
	return startBotWith(t, mutate, state, nil)
}

// startBotWith is startBotState with the computers mk makes over the
// harness's hub; mk may be nil.
func startBotWith(t *testing.T, mutate func(*telegram.Config), state *fakeState, mk func(*fakeHub) hub.Computers) *harness {
	t.Helper()
	return startBotNotices(t, mutate, state, mk, nil)
}

// startBotNotices is startBotWith with a notice store.
func startBotNotices(t *testing.T, mutate func(*telegram.Config), state *fakeState, mk func(*fakeHub) hub.Computers,
	notices telegram.NoticeStore) *harness {
	t.Helper()
	h := &harness{
		tg:     newFakeTelegram(t),
		hub:    newFakeHub(),
		links:  &fakeLinks{users: map[int64]string{aliceTG: userAlice, bobTG: userBob}},
		tokens: &fakeTokens{},
		state:  state,
	}
	cfg := testConfig(h.tg)
	if mutate != nil {
		mutate(&cfg)
	}
	var computers hub.Computers
	if mk != nil {
		computers = mk(h.hub)
	}
	bot, err := telegram.New(cfg, telegram.Options{
		PublicURL: "https://hub.example/",
		Service:   h.hub, Links: h.links, Tokens: h.tokens, State: h.state, Computers: computers, Notices: notices,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.bot = bot
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

func (h *fakeHub) RenameSession(_ context.Context, user, backendID, sessionID, title string) error {
	if err := h.allowed(user, backendID); err != nil {
		return err
	}
	h.update(sessionID, func(s *hub.Session) { s.Title = title })
	return nil
}

// update changes a session of the fake.
func (h *fakeHub) update(sessionID string, f func(*hub.Session)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.sessions {
		if h.sessions[i].ID == sessionID {
			f(&h.sessions[i])
		}
	}
}

// add adds a session to the fake.
func (h *fakeHub) add(s hub.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = append(h.sessions, s)
}

func (h *fakeHub) attachesCopy() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.attaches)
}

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
