package telegram_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/notify"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

const (
	kCommand    = "command"
	kKind       = "kind"
	kQuestion   = "question"
	chatGroup   = "group"
	kPromptID   = "prompt_id"
	kChoices    = "choices"
	beName      = "Cluster"
	mMarkup     = "editMessageReplyMarkup"
	kCallback   = "callback_data"
	msgNotYours = "You are not allowed to answer this request."
	msgGone     = "This request is no longer pending."
)

func data(b map[string]any) string {
	s, _ := b[kCallback].(string)
	return s
}

func labels(c tgCall) []string {
	btns := c.buttons()
	out := make([]string, 0, len(btns))
	for _, b := range btns {
		s, _ := b[kText].(string)
		out = append(out, s)
	}
	return out
}

// waitAnswerText waits for a callback answer with text.
func (f *fakeTelegram) waitAnswerText(text string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.snapshot() {
			if c.Method == mAnswer && c.Params[kText] == text {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("no callback answer %q; calls: %+v", text, f.snapshot())
}

func (h *fakeHub) waitAnswer(t *testing.T) answerCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.answersCopy(); len(got) >= 1 {
			return got[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no answer, got %+v", h.answersCopy())
	return answerCall{}
}

func TestApprovalOffersEveryChoice(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a1", kKind: "approval", kCommand: "ls",
		kChoices: []string{hub.ChoiceOnce, hub.ChoiceDeny, "session", "always"}})
	req := h.tg.wait(mSend, "Needs you")
	if got := labels(req); !slices.Equal(got, []string{lblOnce, lblDeny, "This chat", "Always"}) {
		t.Fatalf("labels = %v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(req.buttons()[3]))
	h.tg.wait(mEdit, "<b>Always approved</b>")
	if got := h.hub.waitAnswer(t); got.Answer.Choice != hub.ChoiceAlways || got.PromptID != "a1" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestMultiSelectQuestion(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindClarifyRequest, map[string]any{kPromptID: "q1", kKind: hub.PromptClarify, kQuestion: "Which <envs>?",
		kChoices: []string{envStaging, envProd, envDev}, "multi_select": true})
	req := h.tg.wait(mSend, "Which &lt;envs&gt;?")
	if got := labels(req); !slices.Equal(got, []string{envStaging, envProd, envDev, "Send", "Other answer", "Skip"}) {
		t.Fatalf("labels = %v", got)
	}
	btns := req.buttons()
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[3]))
	h.tg.waitAnswerText("Pick at least one answer first.")
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[0]))
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[2]))
	marks := h.tg.waitN(mMarkup, "", 2)
	if got := labels(marks[1]); got[0] != "✓ staging" || got[2] != "✓ dev" || got[1] != "prod" {
		t.Fatalf("toggled labels = %v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[3]))
	h.tg.wait(mEdit, "<b>Answered: staging, dev</b>")
	if got := h.hub.waitAnswer(t); !slices.Equal(got.Answer.Picks, []string{envStaging, "dev"}) {
		t.Fatalf("answer = %+v", got)
	}
}

func TestReplyAnswersQuestion(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindClarifyRequest, map[string]any{kPromptID: "q2", kKind: hub.PromptClarify, kQuestion: "Name it", kChoices: []string{}})
	req := h.tg.wait(mSend, "Name it")
	markup, _ := req.Params["reply_markup"].(map[string]any)
	if markup["force_reply"] != true || len(req.buttons()) != 0 {
		t.Fatalf("a question without choices should ask for a reply: %+v", req.Params)
	}
	h.tg.say(dm(aliceTG), aliceTG, "not an answer")
	h.hub.waitPrompt(t, 2)
	h.tg.replyTo(dm(aliceTG), aliceTG, req.ResultID, "Bluebird")
	edit := h.tg.wait(mEdit, "<b>Answered: Bluebird</b>")
	if edit.messageID() != req.ResultID || !strings.Contains(edit.text(), "Name it") {
		t.Fatalf("edit = %+v", edit)
	}
	if got := h.hub.waitAnswer(t); got.Answer.Text != "Bluebird" || got.PromptID != "q2" {
		t.Fatalf("answer = %+v", got)
	}
	if p := h.hub.promptsCopy(); len(p) != 2 || p[1].Text != "not an answer" {
		t.Fatalf("prompts = %+v", p)
	}
	h.tg.replyTo(dm(aliceTG), aliceTG, req.ResultID, "again")
	h.hub.waitPrompt(t, 3)
	if n := len(h.hub.answersCopy()); n != 1 {
		t.Fatalf("a reply to an answered question answered it again: %d answers", n)
	}
}

func TestOtherAnswerAsksForReply(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindClarifyRequest, map[string]any{kPromptID: "q3", kKind: hub.PromptClarify, kQuestion: "Branch?",
		kChoices: []string{"main", "fix-ci"}})
	req := h.tg.wait(mSend, "Branch?")
	if got := labels(req); !slices.Equal(got, []string{"main", "fix-ci", "Other answer", "Skip"}) {
		t.Fatalf("labels = %v", got)
	}
	if !strings.Contains(req.text(), "Or reply to this message") {
		t.Fatalf("question = %q", req.text())
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(req.buttons()[2]))
	ask := h.tg.wait(mSend, "Reply to this message with your answer to")
	if markup, _ := ask.Params["reply_markup"].(map[string]any); markup["force_reply"] != true {
		t.Fatalf("ask = %+v", ask.Params)
	}
	h.tg.replyTo(dm(aliceTG), bobTG, ask.ResultID, "bobs-branch")
	h.tg.wait(mSend, msgNotYours)
	h.tg.replyTo(dm(aliceTG), aliceTG, ask.ResultID, "fix-ci-2")
	edit := h.tg.wait(mEdit, "Answered: fix-ci-2")
	if edit.messageID() != req.ResultID {
		t.Fatalf("edited %d, the question is %d", edit.messageID(), req.ResultID)
	}
	if got := h.hub.waitAnswer(t); got.Answer.Text != "fix-ci-2" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestPromptSettledElsewhere(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a2", kCommand: "ls"})
	req := h.tg.wait(mSend, "Needs you")
	h.hub.emit(t, sid, "approval.settled", map[string]any{kPromptID: "a2"})
	edit := h.tg.wait(mEdit, "<b>Answered elsewhere</b>")
	if edit.messageID() != req.ResultID || edit.Params["reply_markup"] != nil || !strings.Contains(edit.text(), "<pre>ls</pre>") {
		t.Fatalf("edit = %+v", edit)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(req.buttons()[0]))
	h.tg.waitAnswerText(msgGone)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a4", kCommand: "make deploy"})
	h.hub.emit(t, sid, "approval.cancelled", map[string]any{"request_ids": []string{"a4"}})
	h.tg.wait(mEdit, "<b>Cancelled</b>")
}

func TestSnapshotShowsPendingPrompts(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.mu.Lock()
	h.hub.pending = map[string]bool{"a9": true}
	h.hub.mu.Unlock()
	h.hub.emit(t, sid, hub.KindSnapshot, hub.Snapshot{SessionID: sid, Prompts: []json.RawMessage{
		json.RawMessage(`{"kind":"approval","prompt_id":"old","command":"stale step","choices":["once","deny"]}`),
		json.RawMessage(`{"kind":"approval","prompt_id":"a9","command":"open example.com","choices":["once","deny","session","always"]}`),
	}})
	req := h.tg.wait(mSend, "open example.com")
	if got := labels(req); !slices.Equal(got, []string{lblOnce, lblDeny, "This chat", "Always"}) {
		t.Fatalf("labels = %v", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.tg.find(mSend, "stale step"); len(got) != 0 {
		t.Fatalf("a settled prompt of the snapshot was shown: %+v", got)
	}
}

func TestPromptExpires(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a5", kCommand: "migrate", "timeout_s": 0.05})
	req := h.tg.wait(mSend, "Needs you")
	edit := h.tg.wait(mEdit, "<b>Expired</b>")
	if edit.messageID() != req.ResultID || edit.Params["reply_markup"] != nil || !strings.Contains(edit.text(), "migrate") {
		t.Fatalf("edit = %+v", edit)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(req.buttons()[0]))
	h.tg.waitAnswerText(msgGone)
}

type fakeNotices struct {
	mu   sync.Mutex
	recs map[string]hub.NoticeRecord
	msgs map[string][]hub.NoticeMessage
}

func (f *fakeNotices) Notice(_ context.Context, id string) (hub.NoticeRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recs[id]
	return r, ok, nil
}

func (f *fakeNotices) NoticeMessages(_ context.Context, id string) ([]hub.NoticeMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.msgs[id]), nil
}

func (f *fakeNotices) NoticeForMessage(_ context.Context, chatID, messageID int64) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, msgs := range f.msgs {
		for _, m := range msgs {
			if m.ChatID == chatID && m.MessageID == messageID {
				return id, true, nil
			}
		}
	}
	return "", false, nil
}

func TestReplyToStoredNoticeAnswersIt(t *testing.T) {
	t.Parallel()
	rec := hub.NoticeRecord{ID: "nQ", User: userAlice, Kind: hub.NoticeQuestion, BackendID: beCluster, SessionID: "s4",
		PromptID: "q4", Prompt: &hub.PromptInfo{ID: "q4", Kind: hub.PromptClarify, Question: "Which branch?"}}
	notices := &fakeNotices{recs: map[string]hub.NoticeRecord{rec.ID: rec},
		msgs: map[string][]hub.NoticeMessage{rec.ID: {{ChatID: aliceTG, MessageID: 4242}}}}
	h := startBotNotices(t, nil, newFakeState(), nil, notices)
	h.tg.replyTo(dm(aliceTG), aliceTG, 4242, "/stop")
	h.tg.wait(mSend, "No session is attached.")
	h.tg.replyTo(dm(aliceTG), aliceTG, 4242, "fix-ci")
	if got := h.hub.waitAnswer(t); got.Answer.Text != "fix-ci" || got.PromptID != "q4" || got.SessionID != "s4" {
		t.Fatalf("answer = %+v", got)
	}
	if edit := h.tg.wait(mEdit, "Answered: fix-ci"); edit.messageID() != 4242 {
		t.Fatalf("edit = %+v", edit)
	}
	if n := len(h.hub.promptsCopy()); n != 0 {
		t.Fatalf("the reply also went in as a prompt: %d", n)
	}
}

func TestNoticeButtonsAnswerOnlyForTheirPerson(t *testing.T) {
	t.Parallel()
	q := "Ship it?"
	rec := hub.NoticeRecord{ID: "nAbc", User: userAlice, Kind: hub.NoticeQuestion, BackendID: beCluster, SessionID: "s9",
		PromptID: "q9", Prompt: &hub.PromptInfo{ID: "q9", Kind: hub.PromptClarify, Question: q, Choices: []string{"yes", "no"}}}
	notices := &fakeNotices{recs: map[string]hub.NoticeRecord{rec.ID: rec}, msgs: map[string][]hub.NoticeMessage{}}
	h := startBotNotices(t, nil, newFakeState(), nil, notices)
	id, err := h.bot.SendNotice(context.Background(), aliceTG, notify.Message{Record: rec, Title: "Deploy <prod>", Backend: beName,
		URL: "https://hub.example/#/s/cluster/s9"})
	if err != nil || id == 0 {
		t.Fatalf("send = %d, %v", id, err)
	}
	notices.mu.Lock()
	notices.msgs[rec.ID] = []hub.NoticeMessage{{ChatID: aliceTG, MessageID: id}}
	notices.mu.Unlock()
	sent := h.tg.wait(mSend, "Hermes has a question")
	for _, want := range []string{"Deploy &lt;prod&gt;", "Cluster", "Ship it?", `href="https://hub.example/#/s/cluster/s9"`} {
		if !strings.Contains(sent.text(), want) {
			t.Fatalf("notice %q lacks %q", sent.text(), want)
		}
	}
	yes := data(sent.buttons()[0])
	if !strings.HasPrefix(yes, "nq:nAbc:") {
		t.Fatalf("callback = %q", yes)
	}
	h.tg.press(dm(bobTG), bobTG, id, yes)
	h.tg.waitAnswerText(msgNotYours)
	h.tg.press(dm(aliceTG), aliceTG, id+1, yes)
	h.tg.waitAnswerText(msgGone)
	h.tg.press(dm(aliceTG), aliceTG, id, "nq:forged:0")
	if len(h.hub.answersCopy()) != 0 {
		t.Fatalf("answered: %+v", h.hub.answersCopy())
	}
	h.tg.press(dm(aliceTG), aliceTG, id, yes)
	got := h.hub.waitAnswer(t)
	if all := h.hub.answersCopy(); len(all) != 1 || got.User != userAlice || got.SessionID != "s9" || !slices.Equal(got.Answer.Picks, []string{"yes"}) {
		t.Fatalf("answers = %+v", all)
	}
}

func TestSettledShowsNoticeOutcome(t *testing.T) {
	t.Parallel()
	h := startBotNotices(t, nil, newFakeState(), nil, &fakeNotices{})
	h.bot.Settled(context.Background(), hub.NoticeRecord{ID: "n0"}, false, []hub.NoticeMessage{{ChatID: aliceTG, MessageID: 77}})
	c := h.tg.wait(mMarkup, "")
	if c.messageID() != 77 || c.Params["reply_markup"] != nil {
		t.Fatalf("markup edit = %+v", c)
	}
	rec := hub.NoticeRecord{ID: "n1", User: userAlice, Kind: hub.NoticeApproval, BackendID: beCluster, SessionID: "s1",
		PromptID: "a1", Prompt: &hub.PromptInfo{ID: "a1", Kind: hub.PromptApproval, Command: "rm -rf build", Choices: []string{hub.ChoiceOnce, hub.ChoiceDeny}},
		CreatedAt: time.Now()}
	id, err := h.bot.SendNotice(context.Background(), aliceTG, notify.Message{Record: rec, Backend: beName})
	if err != nil {
		t.Fatal(err)
	}
	h.bot.Settled(context.Background(), rec, false, []hub.NoticeMessage{{ChatID: aliceTG, MessageID: id}})
	edit := h.tg.wait(mEdit, "Answered elsewhere")
	if edit.messageID() != id || edit.Params["reply_markup"] != nil || !strings.Contains(edit.text(), "rm -rf build") {
		t.Fatalf("edit = %+v", edit)
	}
	h.bot.Settled(context.Background(), rec, true, []hub.NoticeMessage{{ChatID: aliceTG, MessageID: 90}})
	h.tg.wait(mEdit, "Cancelled")
	rec.Prompt.Timeout, rec.CreatedAt = 60, time.Now().Add(-2*time.Minute)
	h.bot.Settled(context.Background(), rec, false, []hub.NoticeMessage{{ChatID: aliceTG, MessageID: 91}})
	h.tg.wait(mEdit, "No answer in 1m")
}

var _ notify.Telegram = (*telegram.Bot)(nil)

func TestAttachedButtonsOnlyWorkOnTheirMessage(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a3", kCommand: "ls", kChoices: []string{hub.ChoiceOnce, hub.ChoiceDeny}})
	req := h.tg.wait(mSend, "Needs you")
	allow := data(req.buttons()[0])
	if !strings.HasPrefix(allow, "pa:") {
		t.Fatalf("callback = %q", allow)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID+1, allow)
	h.tg.waitAnswerText(msgGone)
	key := strings.Split(allow, ":")[1]
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, "ap:"+key+":0")
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, "ap:1:0")
	time.Sleep(100 * time.Millisecond)
	if got := h.hub.answersCopy(); len(got) != 0 {
		t.Fatalf("answered through a stale or old-format button: %+v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, allow)
	if got := h.hub.waitAnswer(t); got.Answer.Choice != hub.ChoiceOnce {
		t.Fatalf("answer = %+v", got)
	}
}

func TestGroupMemberWithoutAccessCannotToggle(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	group := telegram.Chat{ID: -500, Type: chatGroup}
	h.tg.say(group, aliceTG, "hello")
	sid := h.hub.waitPrompt(t, 1)[0].SessionID
	h.hub.emit(t, sid, hub.KindClarifyRequest, map[string]any{kPromptID: "q5", kKind: hub.PromptClarify, kQuestion: "Which?",
		kChoices: []string{"a", "b"}, "multi_select": true})
	req := h.tg.wait(mSend, "Which?")
	h.tg.press(group, bobTG, req.ResultID, data(req.buttons()[0]))
	h.tg.waitAnswerText(msgNotYours)
	h.tg.press(group, bobTG, req.ResultID, data(req.buttons()[3]))
	h.tg.waitAnswerText(msgNotYours)
}
