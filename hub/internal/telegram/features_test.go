package telegram_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

const (
	mDraft     = "sendMessageDraft"
	mReact     = "setMessageReaction"
	mAction    = "sendChatAction"
	mCommands  = "setMyCommands"
	mAlbum     = "sendMediaGroup"
	mDelCmds   = "deleteMyCommands"
	kScope     = "scope"
	kType      = "type"
	kTodos     = "todos"
	kRevision  = "revision"
	kRequestID = "request_id"
	tripText   = "plan the trip"
	pngData    = "\x89PNG\r\n\x1a\nimg"
	kContent   = "content"
	kMessage   = "message"
	scopeChat  = "chat"
	pdfData    = "%PDF-1.7"
	shotName   = "shot.jpg"
)

// withDrafts turns drafts on.
func withDrafts(c *telegram.Config) { c.Drafts = true }

// reactions are the emoji the bot set, by message, in order.
func (f *fakeTelegram) reactions() []string {
	var out []string
	for _, c := range f.snapshot() {
		if c.Method != mReact {
			continue
		}
		emoji := ""
		if rs, _ := c.Params["reaction"].([]any); len(rs) == 1 {
			r, _ := rs[0].(map[string]any)
			emoji, _ = r["emoji"].(string)
		}
		out = append(out, strings.TrimSpace(strings.Join([]string{itoa(c.messageID()), emoji}, " ")))
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// waitReaction polls until the bot set emoji on message msg.
func (f *fakeTelegram) waitReaction(t *testing.T, msg int64, emoji string) {
	t.Helper()
	want := itoa(msg) + " " + emoji
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(f.reactions(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reaction %q; reactions: %q", want, f.reactions())
}

// ownTurn starts a turn of the prompt the harness sent, as Hermes does.
func (h *harness) ownTurn(t *testing.T, sid string) {
	t.Helper()
	own := h.hub.waitPrompt(t, 1)[0].SubmissionID
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: firstAnswer, kSubmitID: own})
}

func commandNames(c tgCall) []string {
	cmds, _ := c.Params["commands"].([]any)
	out := make([]string, 0, len(cmds))
	for _, x := range cmds {
		m, _ := x.(map[string]any)
		name, _ := m["command"].(string)
		out = append(out, name)
	}
	return out
}

func scopeOf(c tgCall) (string, int64) {
	s, _ := c.Params[kScope].(map[string]any)
	typ, _ := s[kType].(string)
	id, _ := s["chat_id"].(float64)
	return typ, int64(id)
}

func TestCommandMenusAreSet(t *testing.T) {
	t.Parallel()
	h := linkedWith(t, time.Now(), nil)
	private := h.tg.waitCall(mCommands, func(c tgCall) bool { typ, _ := scopeOf(c); return typ == "all_private_chats" })
	for _, want := range []string{"new", "stop", "model", "bg", "status", "link", "unlink", "help"} {
		if !slices.Contains(commandNames(private), want) {
			t.Fatalf("private menu %q lacks %s", commandNames(private), want)
		}
	}
	group := h.tg.waitCall(mCommands, func(c tgCall) bool { typ, _ := scopeOf(c); return typ == "all_group_chats" })
	if got := commandNames(group); !slices.Equal(got, []string{"link", "help"}) {
		t.Fatalf("group menu = %q", got)
	}
	forum := h.tg.waitCall(mCommands, func(c tgCall) bool { typ, id := scopeOf(c); return typ == scopeChat && id == forumID })
	for _, want := range []string{"stop", "model", "bg", "unlink"} {
		if !slices.Contains(commandNames(forum), want) {
			t.Fatalf("linked group menu %q lacks %s", commandNames(forum), want)
		}
	}
	if slices.Contains(commandNames(forum), "link") || slices.Contains(commandNames(forum), "sessions") {
		t.Fatalf("linked group menu = %q", commandNames(forum))
	}
	h.tg.sayIn(forumChat(), 0, aliceTG, "/unlink", nil)
	h.tg.waitCall(mDelCmds, func(c tgCall) bool { typ, id := scopeOf(c); return typ == scopeChat && id == forumID })
}

func TestModelCommand(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.models = hub.Models{Provider: "openai", Model: "gpt-a", Providers: []hub.ModelProvider{
		{ID: "openai", Name: "OpenAI", Models: []hub.ModelChoice{{ID: "gpt-a"}, {ID: "gpt-b"}}},
		{ID: "local", Name: "Local", Models: []hub.ModelChoice{{ID: "qwen"}}},
	}}
	h.hub.mu.Unlock()
	h.startTurn(t)
	h.tg.say(dm(aliceTG), aliceTG, "/model")
	menu := h.tg.wait(mSend, "<b>Model:</b> <code>gpt-a</code>")
	btns := menu.buttons()
	if len(btns) != 3 || btns[0][kText] != "• OpenAI: gpt-a" || btns[2][kText] != "Local: qwen" {
		t.Fatalf("buttons = %+v", btns)
	}
	data, _ := btns[2]["callback_data"].(string)
	h.tg.press(dm(aliceTG), aliceTG, menu.ResultID, data)
	h.tg.wait(mSend, "Model changed to <code>qwen</code>")
	h.tg.say(dm(aliceTG), aliceTG, "/model openai/GPT-B")
	h.tg.waitN(mSend, "Model changed to", 2)
	h.tg.say(dm(aliceTG), aliceTG, "/model nope")
	h.tg.wait(mSend, "No model nope here")
	h.hub.mu.Lock()
	h.hub.modelConfirm = "tok"
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/model qwen")
	ask := h.tg.wait(mSend, "This model costs more.")
	yes, _ := ask.buttons()[0]["callback_data"].(string)
	h.tg.press(dm(aliceTG), aliceTG, ask.ResultID, yes)
	h.tg.waitN(mSend, "Model changed to", 3)
	h.hub.mu.Lock()
	defer h.hub.mu.Unlock()
	want := []string{"local/qwen/", "openai/gpt-b/", "local/qwen/", "local/qwen/tok"}
	if !slices.Equal(h.hub.modelSets, want) {
		t.Fatalf("model sets = %q, want %q", h.hub.modelSets, want)
	}
}

func TestBackgroundCommand(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.tg.say(dm(aliceTG), aliceTG, "/bg")
	h.tg.wait(mSend, "Send /bg and a prompt")
	h.tg.say(dm(aliceTG), aliceTG, "/bg check the build")
	h.tg.wait(mSend, "Started in the background")
	h.hub.mu.Lock()
	bgs := slices.Clone(h.hub.bgs)
	h.hub.mu.Unlock()
	if !slices.Equal(bgs, []string{sid + " check the build"}) {
		t.Fatalf("bg prompts = %q", bgs)
	}
	h.hub.emit(t, sid, "background.complete", map[string]any{"task_id": "bg_1", kText: "The build is **green**."})
	got := h.tg.wait(mSend, "Background answer")
	if !strings.Contains(got.text(), "The build is <b>green</b>.") {
		t.Fatalf("answer = %q", got.text())
	}
}

func TestDraftStreamsTheReplyInAPrivateChat(t *testing.T) {
	t.Parallel()
	h := startBot(t, withDrafts)
	sid := h.startTurn(t)
	h.ownTurn(t, sid)
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "go test ./..."})
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: toolTerminal})
	h.hub.emit(t, sid, hub.KindMessageDelta, map[string]any{kText: "Hello **wor"})
	draft := h.tg.wait(mDraft, "Hello")
	if id, _ := draft.Params["chat_id"].(float64); int64(id) != aliceTG || draft.Params["draft_id"] == nil || draft.Params["parse_mode"] != "HTML" {
		t.Fatalf("draft = %+v", draft.Params)
	}
	if !strings.Contains(draft.text(), "<i><b>Writing…</b>") {
		t.Fatalf("draft has no status line: %q", draft.text())
	}
	h.tg.wait(mSend, "Working…")
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Hello **world**", kStatus: statusComplete})
	h.tg.wait(mSend, "Hello <b>world</b>")
	h.tg.wait(mEdit, "Worked for")
	for _, c := range h.tg.snapshot() {
		if c.Method == mEdit && (strings.Contains(c.text(), "Running") || strings.Contains(c.text(), "Writing")) {
			t.Fatalf("the status line was edited while the turn ran: %q", c.text())
		}
	}
}

func TestDraftFallsBackToEdits(t *testing.T) {
	t.Parallel()
	h := startBot(t, withDrafts)
	h.tg.mu.Lock()
	h.tg.topicErr[mDraft] = "Bad Request: TEXTDRAFT_PEER_INVALID"
	h.tg.mu.Unlock()
	sid := h.startTurn(t)
	h.ownTurn(t, sid)
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "make"})
	h.hub.emit(t, sid, hub.KindToolProgress, map[string]any{kToolID: "t1", "message": "half"})
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: toolTerminal})
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t2", kName: toolTerminal, kContext: "make install"})
	h.tg.wait(mEdit, "Running</b> <b>terminal</b> <code>make install</code>")
}

func TestNoDraftsInGroups(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := startBotFake(t, nil, withDrafts, func() *fakeState {
		st := newFakeState()
		st.groups[forumID] = hub.TelegramGroup{User: userAlice, ChatID: forumID, LinkedAt: since}
		st.topics[topicKey(forumID, beCluster, sFix)] = storedTopic(60, sFix, since)
		return st
	}(), func(fh *fakeHub) hub.Computers {
		fh.sessions = append(fh.sessions, hub.Session{ID: sFix, BackendID: beCluster, Title: sFix, UpdatedAt: since})
		return nil
	}, nil)
	h.tg.sayIn(forumChat(), 60, aliceTG, "hello", nil)
	h.ownTurn(t, sFix)
	h.hub.emit(t, sFix, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "make"})
	h.tg.waitCall(mSend, func(c tgCall) bool { return c.thread() == 60 && strings.Contains(c.text(), "Running") })
	h.hub.emit(t, sFix, hub.KindMessageDelta, map[string]any{kText: "partial"})
	h.hub.emit(t, sFix, hub.KindMessageComplete, map[string]any{kText: "done here", kStatus: statusComplete})
	h.tg.wait(mSend, "done here")
	if n := len(h.tg.find(mDraft, "")); n != 0 {
		t.Fatalf("%d drafts in a group", n)
	}
}

func TestReactionsFollowTheTurn(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.ownTurn(t, sid)
	h.tg.waitReaction(t, 1, "👀")
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kRequestID: "r1", kCommand: "rm x"})
	h.tg.waitReaction(t, 1, "🙏")
	h.hub.emit(t, sid, "approval.settled", map[string]any{kPromptID: "r1"})
	h.tg.waitN(mReact, "", 3)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: firstAnswer, kStatus: statusComplete})
	h.tg.waitReaction(t, 1, "👍")
	if got := h.tg.reactions(); !slices.Equal(got, []string{"1 👀", "1 🙏", "1 👀", "1 👍"}) {
		t.Fatalf("reactions = %q", got)
	}
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "from elsewhere", kSubmitID: hub.NewSubmissionID()})
	echo := h.tg.wait(mSend, "You, in the hub")
	h.tg.waitReaction(t, echo.ResultID, "👀")
	h.hub.emit(t, sid, hub.KindError, map[string]any{kMessage: "boom"})
	h.tg.waitReaction(t, echo.ResultID, "👎")
}

func TestRefusedReactionIsNotRetried(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.mu.Lock()
	h.tg.topicErr[mReact] = "Bad Request: REACTION_INVALID"
	h.tg.mu.Unlock()
	sid := h.startTurn(t)
	h.ownTurn(t, sid)
	h.tg.wait(mReact, "")
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: firstAnswer, kStatus: statusComplete})
	h.tg.wait(mSend, firstAnswer)
	if n := len(h.tg.find(mReact, "")); n != 1 {
		t.Fatalf("%d reactions after a refusal", n)
	}
}

func TestTypingShowsInTheTopicWhileATurnRuns(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := linkedWith(t, since, []string{sFix}, storedTopic(60, sFix, since))
	h.tg.sayIn(forumChat(), 60, aliceTG, "hello", nil)
	h.ownTurn(t, sFix)
	first := h.tg.waitCall(mAction, func(c tgCall) bool { return c.thread() == 60 })
	if first.Params["action"] != "typing" {
		t.Fatalf("action = %+v", first.Params)
	}
	deadline := time.Now().Add(8 * time.Second)
	for len(h.tg.find(mAction, "")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("typing was not shown again")
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.hub.emit(t, sFix, hub.KindMessageComplete, map[string]any{kText: firstAnswer, kStatus: statusComplete})
	h.tg.wait(mSend, firstAnswer)
	n := len(h.tg.find(mAction, ""))
	time.Sleep(5 * time.Second)
	if m := len(h.tg.find(mAction, "")); m != n {
		t.Fatalf("typing went on after the turn: %d, then %d", n, m)
	}
}

func TestAgentMessagesHaveNoLinkPreview(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindThinkingDelta, map[string]any{kText: kPondering})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "See https://example.com/docs", kStatus: statusComplete})
	h.tg.wait(mSend, "https://example.com/docs")
	h.tg.wait(mEdit, "Thought for")
	for _, c := range h.tg.snapshot() {
		if c.Method != mSend && c.Method != mEdit {
			continue
		}
		lp, _ := c.Params["link_preview_options"].(map[string]any)
		if lp["is_disabled"] != true {
			t.Fatalf("%s without link_preview_options.is_disabled: %+v", c.Method, c.Params)
		}
	}
}

func TestReplyImagesGoAsOneAlbum(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{shotPath: {mime: hub.MIMEJPEG, data: jpegData}, "/opt/data/out/plot.png": {mime: "image/png", data: pngData},
		"/opt/data/out/report.pdf": {data: pdfData}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Two pictures.\nMEDIA:" + shotPath +
		"\nMEDIA:/opt/data/out/report.pdf\nMEDIA:/opt/data/out/plot.png", kStatus: statusComplete})
	album := h.tg.wait(mAlbum, "")
	names, _ := album.Params["names"].([]string)
	if !slices.Equal(names, []string{shotName, "plot.png"}) {
		t.Fatalf("album files = %q", names)
	}
	var media []map[string]string
	raw, _ := album.Params["media"].(string)
	if err := json.Unmarshal([]byte(raw), &media); err != nil || len(media) != 2 ||
		media[0][kType] != "photo" || media[0]["media"] != "attach://p0" || media[1]["media"] != "attach://p1" {
		t.Fatalf("media = %v (%v)", album.Params["media"], err)
	}
	h.tg.wait(mDocument, "")
	if n := len(h.tg.find(mPhoto, "")); n != 0 {
		t.Fatalf("%d single photos", n)
	}
}

func TestRefusedAlbumIsSentPhotoByPhoto(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.mu.Lock()
	h.tg.topicErr[mAlbum] = "Bad Request: PHOTO_INVALID_DIMENSIONS"
	h.tg.mu.Unlock()
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{shotPath: {mime: hub.MIMEJPEG, data: jpegData}, "/opt/data/out/plot.png": {mime: "image/png", data: pngData}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "MEDIA:" + shotPath + "\nMEDIA:/opt/data/out/plot.png", kStatus: statusComplete})
	h.tg.waitN(mPhoto, "", 2)
}

func TestTodoListIsTickedOff(t *testing.T) {
	t.Parallel()
	h := startBotNotices(t, nil, newFakeState(), nil, nil, telegram.WithEditGaps(20*time.Millisecond, 20*time.Millisecond))
	sid := h.startTurn(t)
	h.ownTurn(t, sid)
	h.hub.emit(t, sid, "todo.updated", map[string]any{kTodos: []map[string]string{
		{"id": "a", kContent: "Write <tests>", kStatus: "in_progress"},
		{"id": "b", kContent: "Ship", kStatus: "pending"},
		{"id": "c", kContent: "Tag", kStatus: "pending", "parent": "b"},
	}, kRevision: 1})
	first := h.tg.wait(mSend, "<b>To-do</b>")
	if want := "<b>To-do</b> · 0 of 3 done\n☐ <b>Write &lt;tests&gt;</b>\n☐ Ship\n\u2003☐ Tag"; first.text() != want {
		t.Fatalf("list = %q, want %q", first.text(), want)
	}
	h.hub.emit(t, sid, "todo.updated", map[string]any{kTodos: []map[string]string{
		{"id": "a", kContent: "Write <tests>", kStatus: "completed"},
		{"id": "b", kContent: "Ship", kStatus: "in_progress"},
		{"id": "c", kContent: "Tag", kStatus: "cancelled", "parent": "b"},
	}, kRevision: 2})
	edit := h.tg.wait(mEdit, "1 of 3 done")
	if edit.messageID() != first.ResultID || !strings.Contains(edit.text(), "☑ Write &lt;tests&gt;") ||
		!strings.Contains(edit.text(), "☒ <s>Tag</s>") || !strings.Contains(edit.text(), "☐ <b>Ship</b>") {
		t.Fatalf("edit = %+v", edit)
	}
}

// dmState is a store where Alice's private chat has topics, with the
// topic of sFix.
func dmState(since time.Time) *fakeState {
	st := newFakeState()
	st.groups[aliceTG] = hub.TelegramGroup{User: userAlice, ChatID: aliceTG, LinkedAt: since}
	tp := storedTopic(60, sFix, since)
	tp.ChatID = aliceTG
	st.topics[topicKey(aliceTG, beCluster, sFix)] = tp
	return st
}

func dmTopicsOn(f *fakeTelegram) { f.dmTopics = true }

func withSession(since time.Time, ids ...string) func(*fakeHub) hub.Computers {
	return func(fh *fakeHub) hub.Computers {
		for _, id := range ids {
			fh.sessions = append(fh.sessions, hub.Session{ID: id, BackendID: beCluster, Title: id, UpdatedAt: since})
		}
		return nil
	}
}

func TestPrivateChatGetsTopics(t *testing.T) {
	t.Parallel()
	h := startBotFake(t, dmTopicsOn, nil, newFakeState(), nil, nil)
	h.tg.say(dm(aliceTG), aliceTG, tripText)
	topic := h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == tripText })
	if id, _ := topic.Params["chat_id"].(float64); int64(id) != aliceTG {
		t.Fatalf("topic made in chat %v", topic.Params["chat_id"])
	}
	thread := topic.ResultID
	h.state.mu.Lock()
	g, ok := h.state.groups[aliceTG]
	h.state.mu.Unlock()
	if !ok || g.User != userAlice {
		t.Fatalf("groups = %+v", h.state.groups)
	}
	h.tg.wait(mSend, "now gets its own topic here")
	h.tg.waitCall(mSend, func(c tgCall) bool {
		return c.thread() == thread && c.text() == "<i>From the main chat:</i>\n"+tripText
	})
	started := h.tg.wait(mSend, "Started a new chat for that")
	if btns := started.buttons(); len(btns) != 1 || btns[0]["url"] != "https://t.me/"+botName+"/"+itoa(thread) {
		t.Fatalf("buttons = %+v", btns)
	}
	p := h.hub.waitPrompt(t, 1)[0]
	if p.Text != tripText {
		t.Fatalf("prompt = %+v", p)
	}
	h.tg.sayIn(dm(aliceTG), thread, aliceTG, "and the hotel", nil)
	if p2 := h.hub.waitPrompt(t, 2)[1]; p2.SessionID != p.SessionID {
		t.Fatalf("topic message went to %s, not %s", p2.SessionID, p.SessionID)
	}
	h.hub.remove(p.SessionID)
	h.hub.emit(t, p.SessionID, hub.KindSessionDeleted, map[string]any{})
	del := h.tg.waitCall(mDeleteTopic, func(c tgCall) bool { return c.thread() == thread })
	if id, _ := del.Params["chat_id"].(float64); int64(id) != aliceTG {
		t.Fatalf("topic deleted in chat %v", del.Params["chat_id"])
	}
}

func TestPrivateChatTopicOfRunningChatAndArchive(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := startBotFake(t, dmTopicsOn, nil, dmState(since), withSession(since, sFix, "busy"), nil)
	h.hub.update("busy", func(s *hub.Session) { s.Running, s.Title = true, titleLong })
	made := h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == titleLong })
	if id, _ := made.Params["chat_id"].(float64); int64(id) != aliceTG {
		t.Fatalf("topic made in chat %v", made.Params["chat_id"])
	}
	h.hub.update(sFix, func(s *hub.Session) { s.Archived = true })
	h.tg.waitCall(mEditName, func(c tgCall) bool { return c.thread() == 60 && c.name() == sFix+" · archived" })
	h.hub.update(sFix, func(s *hub.Session) { s.Archived = false })
	h.tg.waitCall(mEditName, func(c tgCall) bool { return c.thread() == 60 && c.name() == sFix })
	if n := len(h.tg.find(mCloseTopic, "")) + len(h.tg.find(mReopenTopic, "")); n != 0 {
		t.Fatalf("%d close or reopen calls in a private chat", n)
	}
}

func TestPrivateChatWithLinkedGroupHasNoTopics(t *testing.T) {
	t.Parallel()
	h := startBotFake(t, dmTopicsOn, nil, func() *fakeState {
		st := newFakeState()
		st.groups[forumID] = hub.TelegramGroup{User: userAlice, ChatID: forumID, LinkedAt: time.Now()}
		return st
	}(), nil, nil)
	h.tg.say(dm(aliceTG), aliceTG, "hello")
	h.hub.waitPrompt(t, 1)
	for _, c := range h.tg.snapshot() {
		if id, _ := c.Params["chat_id"].(float64); c.Method == mTopic && int64(id) == aliceTG {
			t.Fatalf("topic in the private chat: %+v", c.Params)
		}
	}
	h.tg.say(dm(aliceTG), aliceTG, "/unlink")
	h.tg.wait(mSend, "Unlinked your group")
	h.state.mu.Lock()
	n := len(h.state.groups)
	h.state.mu.Unlock()
	if n != 0 {
		t.Fatalf("groups after /unlink = %d", n)
	}
	h.tg.say(dm(aliceTG), aliceTG, tripText)
	h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == tripText })
}

func TestPrivateChatTopicsTurnedOff(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour)
	h := startBotFake(t, nil, nil, dmState(since), withSession(since, sFix), nil)
	h.tg.say(dm(aliceTG), aliceTG, "hello")
	p := h.hub.waitPrompt(t, 1)[0]
	if p.SessionID == sFix {
		t.Fatal("the message went to a topic's chat")
	}
	h.state.mu.Lock()
	_, ok := h.state.groups[aliceTG]
	h.state.mu.Unlock()
	if ok {
		t.Fatal("the private chat still has topics")
	}
	if n := len(h.tg.find(mTopic, "")); n != 0 {
		t.Fatalf("%d topics made", n)
	}
}
