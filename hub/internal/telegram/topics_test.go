package telegram_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/notify"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

const (
	forumID       = -1001234567890
	mEditName     = "editForumTopic"
	kThread       = "message_thread_id"
	kName         = "name"
	chatSuperTest = "supergroup"
	kContext      = "context"
	lblDeny       = "Deny"
	envDev        = "dev"
	srcCron       = "cron"
	titleLong     = "Long build"
	branchFixCI   = "fix-ci"
	kToolID       = "tool_id"
	envStaging    = "staging"
	envProd       = "prod"
	lblOnce       = "Once"
	titleFixCI    = "Fix CI"
	topicTrip     = "Trip"
)

func forumChat() telegram.Chat { return telegram.Chat{ID: forumID, Type: chatSuperTest, IsForum: true} }

// sayIn sends text in a topic of a group; thread 0 is General.
func (f *fakeTelegram) sayIn(chat telegram.Chat, thread, from int64, text string, reply *telegram.Message) {
	f.push(telegram.Update{Message: &telegram.Message{
		MessageID: 1, From: &telegram.User{ID: from, FirstName: "u"}, Chat: chat, Text: text,
		MessageThreadID: thread, IsTopicMessage: thread != 0, ReplyToMessage: reply,
	}})
}

func (c tgCall) thread() int64 {
	f, _ := c.Params[kThread].(float64)
	return int64(f)
}

func (c tgCall) name() string {
	s, _ := c.Params[kName].(string)
	return s
}

// waitCall polls until a call of method passes ok.
func (f *fakeTelegram) waitCall(method string, ok func(tgCall) bool) tgCall {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.snapshot() {
			if c.Method == method && ok(c) {
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("no matching %s call; calls: %+v", method, f.snapshot())
	return tgCall{}
}

// linked starts a bot whose state has Alice's group linked since since.
func linked(t *testing.T, since time.Time, topics ...hub.TelegramTopic) *harness {
	t.Helper()
	state := newFakeState()
	state.groups[forumID] = hub.TelegramGroup{User: userAlice, ChatID: forumID, LinkedAt: since}
	for _, tp := range topics {
		state.topics[topicKey(tp.ChatID, tp.BackendID, tp.SessionID)] = tp
	}
	return startBotState(t, nil, state)
}

// waitTopic polls until the state has the topic of a chat.
func (f *fakeState) waitTopic(t *testing.T, chatID int64, backendID, sessionID string) hub.TelegramTopic {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tp, ok := f.topic(chatID, backendID, sessionID); ok {
			return tp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no topic for %s/%s", backendID, sessionID)
	return hub.TelegramTopic{}
}

func TestLinkGroupNeedsTopicsAndRights(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	plain := telegram.Chat{ID: -100777, Type: chatSuperTest}
	h.tg.say(plain, aliceTG, "/link")
	h.tg.wait(mSend, "Turn on Topics")
	h.tg.say(forumChat(), 99, "/link")
	h.tg.wait(mSend, "Link your Telegram account first")
	h.tg.mu.Lock()
	h.tg.member = map[string]any{kStatus: "member"}
	h.tg.mu.Unlock()
	h.tg.say(forumChat(), aliceTG, "/link")
	h.tg.wait(mSend, "Make me an admin")
	h.tg.mu.Lock()
	h.tg.member = map[string]any{kStatus: "administrator", "can_manage_topics": true}
	h.tg.mu.Unlock()
	h.tg.say(forumChat(), aliceTG, "/link")
	h.tg.wait(mSend, "Linked this group to <b>alice</b>")
	h.state.mu.Lock()
	g, ok := h.state.groups[forumID]
	h.state.mu.Unlock()
	if !ok || g.User != userAlice {
		t.Fatalf("groups = %+v", h.state.groups)
	}
	h.tg.sayIn(forumChat(), 0, bobTG, "/unlink", nil)
	h.tg.sayIn(forumChat(), 0, aliceTG, "/unlink", nil)
	h.tg.wait(mSend, "Unlinked.")
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if len(h.state.groups) != 0 {
		t.Fatalf("groups after unlink = %+v", h.state.groups)
	}
}

func TestEachChatGetsATopic(t *testing.T) {
	t.Parallel()
	since := time.Now()
	h := linked(t, since)
	h.hub.add(hub.Session{ID: "old", BackendID: beCluster, Title: "Old chat", UpdatedAt: since.Add(-time.Hour)})
	h.hub.add(hub.Session{ID: "arch", BackendID: beCluster, Title: "Archived", Archived: true, UpdatedAt: since.Add(time.Minute)})
	h.hub.add(hub.Session{ID: "tg", BackendID: beCluster, Title: "Family group", Source: "telegram", UpdatedAt: since.Add(time.Minute)})
	h.hub.add(hub.Session{ID: srcCron, BackendID: beCluster, Title: "SEV-1 review", Source: srcCron, UpdatedAt: since.Add(time.Minute)})
	h.hub.add(hub.Session{ID: "busy", BackendID: beCluster, Title: titleLong, Running: true, UpdatedAt: since.Add(-time.Hour)})
	h.hub.add(hub.Session{ID: "fix", BackendID: beCluster, Title: titleFixCI, UpdatedAt: since.Add(time.Minute)})
	h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == "SEV-1 review · scheduled" })
	h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == titleLong })
	created := h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == titleFixCI })
	thread := created.ResultID
	tp := h.state.waitTopic(t, forumID, beCluster, "fix")
	if tp.ThreadID != thread || tp.Name != titleFixCI || tp.User != userAlice {
		t.Fatalf("topic = %+v", tp)
	}
	h.hub.update("fix", func(s *hub.Session) { s.Title = "Fix the flaky test" })
	h.tg.waitCall(mEditName, func(c tgCall) bool { return c.name() == "Fix the flaky test" && c.thread() == thread })
	h.hub.update("fix", func(s *hub.Session) { s.Running = true })
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(h.hub.attachesCopy(), "fix") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.hub.emit(t, "fix", hub.KindMessageComplete, map[string]any{kText: "CI is green.", kStatus: statusComplete})
	answer := h.tg.wait(mSend, "CI is green.")
	if answer.thread() != thread || answer.Params["chat_id"] != float64(forumID) {
		t.Fatalf("answer went to %+v", answer.Params)
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mTopic && !slices.Contains([]string{titleFixCI, "SEV-1 review · scheduled", titleLong}, c.name()) {
			t.Fatalf("unexpected topic %q", c.name())
		}
	}
}

func TestGeneralStartsANewChat(t *testing.T) {
	t.Parallel()
	h := linked(t, time.Now())
	h.tg.sayIn(forumChat(), 0, aliceTG, "Why is CI red?\nmore detail", nil)
	created := h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == "Why is CI red?" })
	p := h.hub.waitPrompt(t, 1)[0]
	if p.Text != "Why is CI red?\nmore detail" || p.BackendID != beCluster {
		t.Fatalf("prompt = %+v", p)
	}
	echo := h.tg.wait(mSend, "From General")
	if echo.thread() != created.ResultID {
		t.Fatalf("echo went to thread %d, topic is %d", echo.thread(), created.ResultID)
	}
	started := h.tg.wait(mSend, "Started a new chat")
	if started.thread() != 0 || len(started.buttons()) != 1 ||
		started.buttons()[0]["url"] != "https://t.me/c/1234567890/"+strconv.FormatInt(created.ResultID, 10) {
		t.Fatalf("started = %+v", started.Params)
	}
	h.tg.sayIn(forumChat(), created.ResultID, aliceTG, "and now?", nil)
	h.hub.waitPrompt(t, 2)
	h.tg.sayIn(forumChat(), created.ResultID, bobTG, "bob was here", nil)
	h.tg.sayIn(forumChat(), created.ResultID, aliceTG, "last", nil)
	got := h.hub.waitPrompt(t, 3)
	if got[1].SessionID != p.SessionID || got[2].Text != "last" || len(got) != 3 {
		t.Fatalf("prompts = %+v", got)
	}
}

func TestTopicMadeInTelegramGetsAChat(t *testing.T) {
	t.Parallel()
	h := linked(t, time.Now())
	h.tg.sayIn(forumChat(), 555, aliceTG, "plan the trip", &telegram.Message{MessageID: 555,
		ForumTopicCreated: &telegram.ForumTopic{Name: topicTrip}})
	p := h.hub.waitPrompt(t, 1)[0]
	if tp := h.state.waitTopic(t, forumID, p.BackendID, p.SessionID); tp.ThreadID != 555 || tp.Name != topicTrip {
		t.Fatalf("topic = %+v", tp)
	}
	h.hub.mu.Lock()
	defer h.hub.mu.Unlock()
	if h.hub.sessions[0].Title != topicTrip {
		t.Fatalf("session = %+v", h.hub.sessions[0])
	}
}

func TestStoredTopicRoutesAndGoneTopicIsDropped(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linked(t, since, hub.TelegramTopic{ChatID: forumID, ThreadID: 42, User: userAlice, BackendID: beCluster, SessionID: "s7",
		Name: "Deploy", Seen: since})
	h.hub.add(hub.Session{ID: "s7", BackendID: beCluster, Title: "Deploy", UpdatedAt: since})
	h.tg.sayIn(forumChat(), 42, aliceTG, "ship it", nil)
	if p := h.hub.waitPrompt(t, 1)[0]; p.SessionID != "s7" {
		t.Fatalf("prompt = %+v", p)
	}
	h.tg.mu.Lock()
	h.tg.goneThreads[42] = true
	h.tg.mu.Unlock()
	h.hub.emit(t, "s7", hub.KindMessageComplete, map[string]any{kText: "shipped", kStatus: statusComplete})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tp, _ := h.state.topic(forumID, beCluster, "s7"); tp.ThreadID == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a gone topic was not dropped")
}

func TestNoticeInGeneralLinksTheTopic(t *testing.T) {
	t.Parallel()
	since := time.Now()
	h := linked(t, since, hub.TelegramTopic{ChatID: forumID, ThreadID: 42, User: userAlice, BackendID: beCluster, SessionID: "s7",
		Name: "SEV-1 review", Seen: since})
	rec := hub.NoticeRecord{ID: "n1", User: userAlice, Kind: hub.NoticeFailure, BackendID: beCluster, SessionID: "s7"}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.bot.SendNotice(context.Background(), forumID, notify.Message{Record: rec, Backend: beName, Detail: "Needs a sign-in."}); err != nil {
			t.Fatal(err)
		}
		sent := h.tg.find(mSend, "Needs a sign-in.")
		c := sent[len(sent)-1]
		if c.thread() != 0 {
			t.Fatalf("notice went to thread %d", c.thread())
		}
		if btns := c.buttons(); len(btns) == 1 && btns[0]["url"] == "https://t.me/c/1234567890/42" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no topic button; calls: %+v", h.tg.snapshot())
}

const (
	mDeleteTopic = "deleteForumTopic"
	mCloseTopic  = "closeForumTopic"
	mReopenTopic = "reopenForumTopic"
	sFix         = "fix"
	sGone        = "gone"
)

func storedTopic(thread int64, sessionID string, since time.Time) hub.TelegramTopic {
	return hub.TelegramTopic{ChatID: forumID, ThreadID: thread, User: userAlice, BackendID: beCluster, SessionID: sessionID,
		Name: sessionID, Seen: since}
}

// waitNoTopic polls until the store has no topic for a session.
func (f *fakeState) waitNoTopic(t *testing.T, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := f.topic(forumID, beCluster, sessionID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the topic of %s is still stored", sessionID)
}

func TestDeletedChatDeletesItsTopic(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linked(t, since, storedTopic(42, "s7", since))
	h.hub.add(hub.Session{ID: "s7", BackendID: beCluster, Title: "s7", UpdatedAt: since})
	h.tg.sayIn(forumChat(), 42, aliceTG, "hello", nil)
	h.hub.waitPrompt(t, 1)
	h.hub.remove("s7")
	h.hub.emit(t, "s7", hub.KindSessionDeleted, map[string]any{})
	h.tg.waitCall(mDeleteTopic, func(c tgCall) bool { return c.thread() == 42 })
	h.state.waitNoTopic(t, "s7")
	if got := h.tg.find(mSend, "deleted"); len(got) != 0 {
		t.Fatalf("a deleted chat's topic got a message: %q", got[0].text())
	}
}

func TestReconcileDeletesTopicsOfGoneChats(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linked(t, since, storedTopic(50, sGone, since), storedTopic(51, "kept", since), storedTopic(0, "dropped", since),
		storedTopic(1, "general", since))
	h.hub.add(hub.Session{ID: "kept", BackendID: beCluster, Title: "kept", UpdatedAt: since})
	h.tg.waitCall(mDeleteTopic, func(c tgCall) bool { return c.thread() == 50 })
	for _, id := range []string{sGone, "dropped", "general"} {
		h.state.waitNoTopic(t, id)
	}
	if _, ok := h.state.topic(forumID, beCluster, "kept"); !ok {
		t.Fatal("the topic of a chat that exists was deleted")
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mDeleteTopic && c.thread() != 50 {
			t.Fatalf("deleted topic %d", c.thread())
		}
	}
}

func TestReconcileKeepsTopicsWhenItCannotCheck(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	state := newFakeState()
	state.groups[forumID] = hub.TelegramGroup{User: userAlice, ChatID: forumID, LinkedAt: since}
	tp := storedTopic(50, "unknown", since)
	state.topics[topicKey(tp.ChatID, tp.BackendID, tp.SessionID)] = tp
	h := startBotWith(t, nil, state, func(fh *fakeHub) hub.Computers {
		fh.checkErr = hub.ErrUnknownBackend
		return nil
	})
	h.hub.add(hub.Session{ID: "other", BackendID: beCluster, Title: "Other", UpdatedAt: time.Now()})
	h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == "Other" })
	if _, ok := h.state.topic(forumID, beCluster, "unknown"); !ok || len(h.tg.find(mDeleteTopic, "")) != 0 {
		t.Fatal("a topic was deleted although its chat could not be checked")
	}
}

func TestArchiveClosesAndUnarchiveReopensTheTopic(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linked(t, since, storedTopic(60, sFix, since))
	h.hub.add(hub.Session{ID: sFix, BackendID: beCluster, Title: sFix, UpdatedAt: since})
	h.hub.update(sFix, func(s *hub.Session) { s.Archived = true })
	h.tg.waitCall(mCloseTopic, func(c tgCall) bool { return c.thread() == 60 })
	waitClosed(t, h.state, sFix, true)
	h.hub.update(sFix, func(s *hub.Session) { s.Archived = false })
	h.tg.waitCall(mReopenTopic, func(c tgCall) bool { return c.thread() == 60 })
	waitClosed(t, h.state, sFix, false)
	if n := len(h.tg.find(mCloseTopic, "")); n != 1 {
		t.Fatalf("closed %d times", n)
	}
}

func waitClosed(t *testing.T, st *fakeState, sessionID string, closed bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tp, ok := st.topic(forumID, beCluster, sessionID); ok && tp.Closed == closed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("topic of %s never became closed=%v", sessionID, closed)
}

// deleteViaEvent attaches the topic of a chat and deletes the chat, as a
// delete in the hub does.
func deleteViaEvent(t *testing.T, h *harness, thread int64, sessionID string, n int) {
	t.Helper()
	h.tg.sayIn(forumChat(), thread, aliceTG, "hello", nil)
	h.hub.waitPrompt(t, n)
	h.hub.remove(sessionID)
	h.hub.emit(t, sessionID, hub.KindSessionDeleted, map[string]any{})
}

func TestTopicErrors(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linked(t, since, storedTopic(70, "a", since), storedTopic(71, "b", since))
	h.hub.add(hub.Session{ID: "a", BackendID: beCluster, Title: "a", UpdatedAt: since})
	h.hub.add(hub.Session{ID: "b", BackendID: beCluster, Title: "b", UpdatedAt: since})
	h.tg.mu.Lock()
	h.tg.topicErr[mDeleteTopic] = "Bad Request: not enough rights to manage topics in the chat"
	h.tg.mu.Unlock()
	deleteViaEvent(t, h, 70, "a", 1)
	rights := h.tg.wait(mSend, "Manage Topics")
	if rights.thread() != 0 || !strings.Contains(rights.text(), "Delete Messages") {
		t.Fatalf("rights notice = %+v", rights.Params)
	}
	deleteViaEvent(t, h, 71, "b", 2)
	h.tg.waitCall(mDeleteTopic, func(c tgCall) bool { return c.thread() == 71 })
	if _, ok := h.state.topic(forumID, beCluster, "a"); !ok {
		t.Fatal("a topic the bot could not delete was forgotten")
	}
	if n := len(h.tg.find(mSend, "Manage Topics")); n != 1 {
		t.Fatalf("rights notice sent %d times", n)
	}
}

func TestTopicAlreadyDeletedIsForgotten(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linked(t, since, storedTopic(80, sGone, since))
	h.hub.add(hub.Session{ID: sGone, BackendID: beCluster, Title: sGone, UpdatedAt: since})
	h.tg.mu.Lock()
	h.tg.topicErr[mDeleteTopic] = "Bad Request: TOPIC_ID_INVALID"
	h.tg.mu.Unlock()
	deleteViaEvent(t, h, 80, sGone, 1)
	h.state.waitNoTopic(t, sGone)
}
