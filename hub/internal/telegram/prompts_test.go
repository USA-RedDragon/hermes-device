package telegram_test

import (
	"context"
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
		kChoices: []string{"once", "deny", "session", "always"}})
	req := h.tg.wait(mSend, "Approval needed")
	if got := labels(req); !slices.Equal(got, []string{"Allow", "Deny", "Allow in this chat", "Always allow"}) {
		t.Fatalf("labels = %v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(req.buttons()[3]))
	h.tg.wait(mEdit, "Always allowed by alice")
	if got := h.hub.waitAnswer(t); got.Answer.Choice != hub.ChoiceAlways || got.PromptID != "a1" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestMultiSelectQuestion(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindClarifyRequest, map[string]any{kPromptID: "q1", kKind: hub.PromptClarify, kQuestion: "Which <envs>?",
		kChoices: []string{"staging", "prod", "dev"}, "multi_select": true})
	req := h.tg.wait(mSend, "Which &lt;envs&gt;?")
	if got := labels(req); !slices.Equal(got, []string{"1. staging", "2. prod", "3. dev", "Send", "Type an answer", "Skip"}) {
		t.Fatalf("labels = %v", got)
	}
	btns := req.buttons()
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[3]))
	h.tg.waitAnswerText("Pick at least one answer first.")
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[0]))
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[2]))
	marks := h.tg.waitN(mMarkup, "", 2)
	if got := labels(marks[1]); got[0] != "✓ 1. staging" || got[2] != "✓ 3. dev" || got[1] != "2. prod" {
		t.Fatalf("toggled labels = %v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(btns[3]))
	h.tg.wait(mEdit, "Answered: staging, dev by alice")
	if got := h.hub.waitAnswer(t); !slices.Equal(got.Answer.Picks, []string{"staging", "dev"}) {
		t.Fatalf("answer = %+v", got)
	}
}

func TestTypedAnswer(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindClarifyRequest, map[string]any{kPromptID: "q2", kKind: hub.PromptClarify, kQuestion: "Name it", kChoices: []string{}})
	req := h.tg.wait(mSend, "Name it")
	if got := labels(req); !slices.Equal(got, []string{"Answer", "Skip"}) {
		t.Fatalf("labels = %v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, req.ResultID, data(req.buttons()[0]))
	h.tg.wait(mSend, "Send your answer")
	h.tg.say(dm(aliceTG), aliceTG, "Bluebird")
	h.tg.wait(mSend, "Sent your answer.")
	if got := h.hub.waitAnswer(t); got.Answer.Text != "Bluebird" || got.PromptID != "q2" {
		t.Fatalf("answer = %+v", got)
	}
	if n := len(h.hub.promptsCopy()); n != 1 {
		t.Fatalf("the answer also went in as a prompt: %d prompts", n)
	}
}

func TestPromptSettledElsewhere(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a2", kCommand: "ls"})
	req := h.tg.wait(mSend, "Approval needed")
	h.hub.emit(t, sid, "approval.settled", map[string]any{kPromptID: "a2"})
	edit := h.tg.wait(mEdit, "Answered.")
	if edit.messageID() != req.ResultID {
		t.Fatalf("edited %d, want %d", edit.messageID(), req.ResultID)
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

func TestSettledTakesNoticeButtonsOff(t *testing.T) {
	t.Parallel()
	h := startBotNotices(t, nil, newFakeState(), nil, &fakeNotices{})
	h.bot.Settled(context.Background(), []hub.NoticeMessage{{ChatID: aliceTG, MessageID: 77}})
	c := h.tg.wait(mMarkup, "")
	if c.messageID() != 77 || c.Params["reply_markup"] != nil {
		t.Fatalf("markup edit = %+v", c)
	}
}

var _ notify.Telegram = (*telegram.Bot)(nil)

func TestAttachedButtonsOnlyWorkOnTheirMessage(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{kPromptID: "a3", kCommand: "ls", kChoices: []string{"once", "deny"}})
	req := h.tg.wait(mSend, "Approval needed")
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
