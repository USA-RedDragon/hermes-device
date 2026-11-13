package telegram_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/telegram"
	"github.com/google/uuid"
)

func TestUnknownUserGetsLink(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.say(dm(77), 77, "hi")
	c := h.tg.wait(mSend, "not linked")
	if !strings.Contains(c.text(), `href="https://hub.example/link/telegram?token=tok-77%26x"`) {
		t.Fatalf("link message = %q", c.text())
	}
	if p := h.hub.promptsCopy(); len(p) != 0 {
		t.Fatalf("prompted for unlinked user: %+v", p)
	}
}

func TestUnknownUserInGroupGetsNoLink(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.say(telegram.Chat{ID: -999, Type: chatSuperTest}, 77, "ignored group")
	h.tg.say(telegram.Chat{ID: -500, Type: "group"}, 77, "allowed group")
	h.tg.wait(mSend, "direct message")
	if got := h.tg.find(mSend, "token="); len(got) != 0 {
		t.Fatalf("posted a link token in a group: %+v", got)
	}
	for _, c := range h.tg.snapshot() {
		if id, _ := c.Params["chat_id"].(float64); id == -999 {
			t.Fatalf("answered in a group that is not allowed: %+v", c)
		}
	}
	h.tokens.mu.Lock()
	defer h.tokens.mu.Unlock()
	if len(h.tokens.created) != 0 {
		t.Fatalf("created tokens: %v", h.tokens.created)
	}
}

func TestRoutesToLinkedUsersDefaultBackend(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.say(dm(aliceTG), aliceTG, "from alice")
	h.hub.waitPrompt(t, 1)
	h.tg.say(dm(bobTG), bobTG, "from bob")
	p := h.hub.waitPrompt(t, 2)
	if p[0].User != userAlice || p[0].BackendID != beCluster || p[0].Text != "from alice" {
		t.Fatalf("alice prompt = %+v", p[0])
	}
	if p[1].User != userBob || p[1].BackendID != beHers {
		t.Fatalf("bob prompt = %+v", p[1])
	}
	h.tg.say(dm(aliceTG), aliceTG, "again")
	p = h.hub.waitPrompt(t, 3)
	if p[2].SessionID != p[0].SessionID {
		t.Fatalf("second prompt went to a new session: %+v", p)
	}
}

func TestAuthorizationDenialIsReported(t *testing.T) {
	t.Parallel()
	h := startBot(t, func(c *telegram.Config) { c.DefaultBackends = []string{"bob=cluster"} })
	h.tg.say(dm(bobTG), bobTG, "try alice's backend")
	h.tg.wait(mSend, "do not have access")
	h.tg.say(dm(bobTG), bobTG, "/new cluster")
	h.tg.wait(mSend, "Unknown backend")
	if p := h.hub.promptsCopy(); len(p) != 0 {
		t.Fatalf("prompt went through: %+v", p)
	}
}

func TestReasoningEditsAreThrottled(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	for i := range 30 {
		h.hub.emit(t, sid, hub.KindReasoningDelta, map[string]any{kText: "step " + string(rune('a'+i%26)) + " <x> "})
		time.Sleep(20 * time.Millisecond)
	}
	h.hub.emit(t, sid, hub.KindReasoningDelta, map[string]any{kText: "LAST"})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "done", kStatus: statusComplete})
	h.tg.wait(mSend, "done")

	first := h.tg.wait(mSend, "Thinking")
	var edits []tgCall
	for _, c := range h.tg.snapshot() {
		if c.Method == mEdit && c.messageID() == first.ResultID {
			edits = append(edits, c)
		}
	}
	if len(edits) == 0 || len(edits) > 5 {
		t.Fatalf("got %d edits for 31 deltas over ~600ms, want 1..5", len(edits))
	}
	for i := 1; i < len(edits)-1; i++ {
		if gap := edits[i].At.Sub(edits[i-1].At); gap < 250*time.Millisecond {
			t.Fatalf("edit %d came %v after the previous one", i, gap)
		}
	}
	last := edits[len(edits)-1].text()
	if !strings.HasPrefix(last, "<i>Thought for") || !strings.Contains(last, "<blockquote expandable>") ||
		!strings.Contains(last, "LAST") || !strings.Contains(last, "&lt;x&gt;") {
		t.Fatalf("final thinking edit = %q", last)
	}
}

func TestStatusLineCollectsSteps(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindReasoningDelta, map[string]any{kText: "look at the files"})
	status := h.tg.wait(mSend, "Thinking…")
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "ls -la /srv"})
	h.tg.wait(mEdit, "<b>Running</b> <b>terminal</b> <code>ls -la /srv</code>")
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: toolTerminal, "duration_s": 1.234, "summary": "3 files"})
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t2", kName: "patch", kContext: "main.go"})
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t2", kName: "patch"})
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t3", kName: toolTerminal, kContext: "go test"})
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t3", kName: toolTerminal})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "All fixed.", kStatus: statusComplete})
	final := h.tg.wait(mEdit, "Worked for")
	if final.messageID() != status.ResultID {
		t.Fatalf("final status edited %d, the status line is %d", final.messageID(), status.ResultID)
	}
	for _, want := range []string{"ran 2 commands, edited 1 file", "<blockquote expandable>",
		"✓ <b>terminal</b> <code>ls -la /srv</code> <i>(1.2s)</i> 3 files", "✓ <b>patch</b>", "look at the files"} {
		if !strings.Contains(final.text(), want) {
			t.Fatalf("final status %q lacks %q", final.text(), want)
		}
	}
	answer := h.tg.wait(mSend, "All fixed.")
	if answer.ResultID == status.ResultID || answer.text() != "All fixed." {
		t.Fatalf("answer = %+v", answer)
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && c.ResultID != status.ResultID && c.ResultID != answer.ResultID && c.text() != "" &&
			!strings.Contains(c.text(), "hello") {
			t.Fatalf("a step got a message of its own: %q", c.text())
		}
	}
}

func TestStatusLineStepsOff(t *testing.T) {
	t.Parallel()
	h := startBot(t, func(c *telegram.Config) { c.Steps = telegram.StepsOff })
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindReasoningDelta, map[string]any{kText: "secret plan"})
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: "web_search", kContext: "golang"})
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: "web_search"})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Found it.", kStatus: statusComplete})
	final := h.tg.wait(mEdit, "Worked for")
	if !strings.Contains(final.text(), "searched 1 time") || strings.Contains(final.text(), "blockquote") ||
		strings.Contains(final.text(), "secret plan") {
		t.Fatalf("final status = %q", final.text())
	}
}

func TestCronResultIsSent(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "first answer", kStatus: statusComplete})
	h.tg.wait(mSend, "first answer")
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "a user message", "submission_id": h.hub.waitPrompt(t, 1)[0].SubmissionID})
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "Cronjob Response: **ping**", "note": srcCron})
	got := h.tg.wait(mSend, "Cronjob Response")
	if got.text() != "Cronjob Response: <b>ping</b>" {
		t.Fatalf("cron result = %q", got.text())
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && strings.Contains(c.text(), "a user message") {
			t.Fatalf("user message echoed: %q", c.text())
		}
	}
}

func TestAgentMessageIsSent(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "first answer", kStatus: statusComplete})
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "[Agent message am_0123456789abcdef from chat 'build' " +
		"(session s1)]\nAnother AI agent of this user wrote everything below this line.\n\nTests **pass** <now>"})
	got := h.tg.wait(mSend, "Agent message from")
	if got.text() != "<b>Agent message from chat &#39;build&#39; (session s1)</b>\nTests <b>pass</b> &lt;now&gt;" {
		t.Fatalf("agent message = %q", got.text())
	}
}

func TestFinalAnswerIsSplit(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	long := strings.Repeat("word & <tag> ", 900)
	h.hub.emit(t, sid, hub.KindMessageDelta, map[string]any{kText: "partial"})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: long, kStatus: statusComplete})
	want := len(telegram.RenderMarkdown(long, telegram.MaxMessageLen))
	if want < 3 {
		t.Fatalf("expected at least 3 chunks, RenderMarkdown gives %d", want)
	}
	chunks := h.tg.waitN(mSend, "word", want)
	var joined strings.Builder
	for _, c := range chunks {
		if n := len(utf16.Encode([]rune(c.text()))); n > telegram.MaxMessageLen {
			t.Fatalf("chunk is %d UTF-16 units", n)
		}
		joined.WriteString(c.text())
	}
	if got := strings.Count(joined.String(), "&amp;"); got != 900 {
		t.Fatalf("got %d escaped ampersands, want 900", got)
	}
	if strings.Contains(joined.String(), "<tag>") {
		t.Fatal("raw tag leaked into HTML")
	}
}

func TestApprovalButtons(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{
		"request_id": "r1", "command": "rm -rf /srv/x", kDesc: "delete files", "tool_name": toolTerminal,
	})
	req := h.tg.wait(mSend, "Needs you")
	btns := req.buttons()
	if len(btns) != 2 || btns[0][kText] != lblOnce || btns[1][kText] != lblDeny {
		t.Fatalf("buttons = %+v", btns)
	}
	msgID := req.ResultID
	allow, _ := btns[0]["callback_data"].(string)
	h.tg.press(dm(aliceTG), aliceTG, msgID, allow)
	edit := h.tg.wait(mEdit, "<b>Approved once</b>")
	if !strings.Contains(edit.text(), "rm -rf /srv/x") || !strings.Contains(edit.text(), "By alice.") {
		t.Fatalf("approval edit = %q", edit.text())
	}
	if edit.messageID() != msgID || edit.Params["reply_markup"] != nil {
		t.Fatalf("approval edit = %+v", edit)
	}
	h.hub.mu.Lock()
	got := h.hub.approvals
	h.hub.mu.Unlock()
	if len(got) != 1 || got[0] != (approvalCall{userAlice, beCluster, sid, "r1", true}) {
		t.Fatalf("approvals = %+v", got)
	}
	h.tg.press(dm(aliceTG), aliceTG, msgID, allow)
	h.tg.wait(mAnswer, "")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range h.tg.snapshot() {
			if c.Method == mAnswer && c.Params[kText] == "This request is no longer pending." {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("second press was not refused")
}

func TestApprovalDeniedForOtherUser(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	group := telegram.Chat{ID: -500, Type: "group"}
	h.tg.say(group, aliceTG, "hello")
	sid := h.hub.waitPrompt(t, 1)[0].SessionID
	h.hub.emit(t, sid, hub.KindApprovalRequest, map[string]any{"prompt_id": "r9", "command": "reboot"})
	req := h.tg.wait(mSend, "Needs you")
	data, _ := req.buttons()[0]["callback_data"].(string)
	h.tg.press(group, bobTG, req.ResultID, data)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range h.tg.snapshot() {
			if c.Method == mAnswer && c.Params[kText] == "You are not allowed to answer this request." {
				h.hub.mu.Lock()
				defer h.hub.mu.Unlock()
				if len(h.hub.approvals) != 0 {
					t.Fatalf("approval went through: %+v", h.hub.approvals)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no denial answer; calls: %+v", h.tg.snapshot())
}

func TestStop(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.say(dm(aliceTG), aliceTG, "/stop")
	h.tg.wait(mSend, "No session is attached")
	sid := h.startTurn(t)
	h.tg.say(dm(aliceTG), aliceTG, "/stop@hermes_bot")
	h.tg.wait(mSend, "Stop requested")
	h.hub.mu.Lock()
	defer h.hub.mu.Unlock()
	if len(h.hub.interrupts) != 1 || h.hub.interrupts[0] != sid {
		t.Fatalf("interrupts = %v", h.hub.interrupts)
	}
}

func TestSessionsAndAttachButton(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.sessions = []hub.Session{
		{ID: sessOld, BackendID: beLaptop, Title: "Old", UpdatedAt: time.Unix(100, 0)},
		{ID: "new", BackendID: beCluster, Title: "New <b>", Live: true, UpdatedAt: time.Unix(200, 0)},
		{ID: "hers1", BackendID: beHers, Title: "Hers", UpdatedAt: time.Unix(300, 0)},
	}
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/sessions")
	list := h.tg.wait(mSend, "<b>Sessions</b>")
	if strings.Contains(list.text(), "Hers") || !strings.Contains(list.text(), "1. New &lt;b&gt;") {
		t.Fatalf("session list = %q", list.text())
	}
	btns := list.buttons()
	if len(btns) != 2 {
		t.Fatalf("buttons = %+v", btns)
	}
	data, _ := btns[1]["callback_data"].(string)
	h.tg.press(dm(aliceTG), aliceTG, list.ResultID, data)
	h.tg.wait(mSend, "Attached to <b>Old</b>")
	h.tg.say(dm(aliceTG), aliceTG, "continue")
	p := h.hub.waitPrompt(t, 1)
	if p[0].SessionID != sessOld || p[0].BackendID != beLaptop {
		t.Fatalf("prompt = %+v", p[0])
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.mu.Lock()
	h.tg.fail429[mSend] = 1
	h.tg.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/help")
	tries := h.tg.waitN(mSend, "Signed in as", 2)
	if gap := tries[1].At.Sub(tries[0].At); gap < time.Second {
		t.Fatalf("retried after %v, want at least retry_after", gap)
	}
	if tries[0].ResultID != 0 || tries[1].ResultID == 0 {
		t.Fatalf("tries = %+v", tries)
	}
}

func TestRejectedHTMLFallsBackToPlain(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.mu.Lock()
	h.tg.rejectHTML = 1
	h.tg.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "use **bold** & `code`"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range h.tg.snapshot() {
			if c.Method == mSend && c.Params["parse_mode"] == nil && c.text() == "use bold & code" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no plain fallback; calls: %+v", h.tg.snapshot())
}

func TestInterruptedAndErrorEvents(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageDelta, map[string]any{kText: "half an ans"})
	h.hub.emit(t, sid, hub.KindMessageDelta, map[string]any{kText: nil})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "", kStatus: "interrupted"})
	h.tg.wait(mSend, "half an ans\n\n<b>Interrupted.</b>")
	h.hub.emit(t, sid, hub.KindError, map[string]any{"message": "model <down>"})
	h.tg.wait(mSend, "<b>Error:</b> model &lt;down&gt;")
}

func TestSessionsNamesUnreachableBackends(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.sessions = []hub.Session{{ID: sessOld, BackendID: beLaptop, Title: "Old", UpdatedAt: time.Unix(100, 0)}}
	h.hub.down = map[string]string{beCluster: "dial tcp: connection refused"}
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/sessions")
	list := h.tg.wait(mSend, "<b>Sessions</b>")
	if !strings.Contains(list.text(), "1. Old") || !strings.Contains(list.text(), "Can't reach Cluster right now, so its sessions are missing.") {
		t.Fatalf("session list = %q", list.text())
	}
	h.hub.mu.Lock()
	h.hub.sessions = nil
	h.hub.down[beLaptop] = "dial tcp: connection refused"
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/sessions")
	h.tg.wait(mSend, "Can't reach Laptop, Cluster right now. It may be restarting; try again in a minute.")
}

func TestHeldPromptIsAnnounced(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.hold = true
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.tg.wait(mSend, "Hermes is restarting. Your message will send when it is back.")
	h.hub.emit(t, sid, hub.KindInputNotSent, hub.HeldInput{ID: "p1", Text: "hello", Reason: hub.NotSentTimeout,
		Message: "Hermes did not come back in time."})
	c := h.tg.wait(mSend, "Your message was not sent")
	if !strings.Contains(c.text(), "did not come back in time") || !strings.Contains(c.text(), "hello") {
		t.Fatalf("not sent message = %q", c.text())
	}
}

const (
	sOld    = "s-old"
	sessOld = "old"
)

func TestRestoresChatsAndOffset(t *testing.T) {
	t.Parallel()
	st := newFakeState()
	st.offset = 41
	st.chats[aliceTG] = hub.TelegramChat{ChatID: aliceTG, User: userAlice, BackendID: beLaptop, SessionID: sOld}
	h := startBotState(t, nil, st)
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.hub.mu.Lock()
		attached := len(h.hub.attaches) > 0 && h.hub.attaches[0] == sOld
		h.hub.mu.Unlock()
		if attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stored chat not attached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.tg.say(dm(aliceTG), aliceTG, "carry on")
	p := h.hub.waitPrompt(t, 1)
	if p[0].SessionID != sOld || p[0].BackendID != beLaptop {
		t.Fatalf("prompt = %+v", p[0])
	}
	want := fmt.Sprintf("tg:%d:0:", aliceTG) + uuid.NewSHA1(uuid.NameSpaceURL, []byte("hermes-hub:telegram:123:1")).String()
	if p[0].SubmissionID != want {
		t.Fatalf("submission id = %q, want %q", p[0].SubmissionID, want)
	}
	h.tg.mu.Lock()
	first := h.tg.offsets[0]
	h.tg.mu.Unlock()
	if first != 41 {
		t.Fatalf("first getUpdates offset = %d", first)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.offset != 2 {
		t.Fatalf("stored offset = %d, want 2", st.offset)
	}
	if c := st.chats[aliceTG]; c.SessionID != sOld {
		t.Fatalf("stored chat = %+v", c)
	}
}

func TestStatusLineCountsTokens(t *testing.T) {
	t.Parallel()
	const kInput, kOutput = "input", "output"
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "go test"})
	h.tg.wait(mSend, "Running")
	h.hub.emit(t, sid, "session.usage", map[string]any{"usage": map[string]any{"prompt": 12000}, hub.TurnKey: map[string]any{kInput: 12000, kOutput: 900}})
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: toolTerminal})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Done.", kStatus: statusComplete,
		hub.TurnKey: map[string]any{kInput: 31234, kOutput: 2800}})
	final := h.tg.wait(mEdit, "Worked for")
	if !strings.Contains(final.text(), "ran 1 command · 31k in, 3k out</i>") {
		t.Fatalf("final status = %q", final.text())
	}
	sid = h.startTurn(t)
	h.hub.emit(t, sid, hub.KindReasoningDelta, map[string]any{kText: "hmm"})
	h.tg.wait(mSend, "Thinking…")
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Ok.", kStatus: statusComplete,
		hub.TurnKey: map[string]any{kInput: nil, kOutput: 40}})
	final = h.tg.wait(mEdit, "Thought for")
	if !strings.Contains(final.text(), " · 40 out</i>") || strings.Contains(final.text(), " in,") {
		t.Fatalf("final status without input = %q", final.text())
	}
}

// TestRunningStatusHasNoQuote checks that the status line edited while a
// turn runs never holds an expandable quote, which Telegram folds again on
// every edit, and that its Steps button sends the steps as a new message.
func TestRunningStatusHasNoQuote(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindReasoningDelta, map[string]any{kText: "plan the fix"})
	status := h.tg.wait(mSend, "Thinking…")
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "go test ./..."})
	h.tg.wait(mEdit, "<b>Running</b> <b>terminal</b> <code>go test ./...</code>")
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: toolTerminal, "summary": "ok"})
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t2", kName: toolPatch, kContext: "main.go"})
	running := h.tg.wait(mEdit, "1 step · ")
	btns := running.buttons()
	if len(btns) != 1 || btns[0][kText] != "Steps" {
		t.Fatalf("running status buttons = %v", btns)
	}
	h.tg.press(dm(aliceTG), aliceTG, status.ResultID, data(btns[0]))
	snap := h.tg.wait(mSend, "Steps so far")
	if snap.ResultID == status.ResultID || !strings.Contains(snap.text(), "<blockquote expandable>") ||
		!strings.Contains(snap.text(), "✓ <b>terminal</b> <code>go test ./...</code> ok") || !strings.Contains(snap.text(), "plan the fix") {
		t.Fatalf("steps message = %q", snap.text())
	}
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t2", kName: toolPatch})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Fixed.", kStatus: statusComplete})
	final := h.tg.wait(mEdit, "Worked for")
	if !strings.Contains(final.text(), "<blockquote expandable>") || len(final.buttons()) != 0 {
		t.Fatalf("final status = %q, buttons %v", final.text(), final.buttons())
	}
	for _, c := range h.tg.snapshot() {
		if c.ResultID == status.ResultID || (c.Method == mEdit && c.messageID() == status.ResultID && c.text() != final.text()) {
			if strings.Contains(c.text(), "blockquote") {
				t.Fatalf("running status holds a quote: %q", c.text())
			}
		}
	}
	h.tg.press(dm(aliceTG), aliceTG, status.ResultID, cbStepsData)
	h.tg.wait(mAnswer, "This turn is done")
	for _, c := range h.tg.snapshot() {
		if c.Method == mEdit && c.messageID() == status.ResultID && c.At.After(final.At) {
			t.Fatalf("status edited after the final summary: %q", c.text())
		}
	}
}

// cbStepsData is the callback data of the Steps button, toolPatch a
// tool that edits a file.
const (
	cbStepsData = "ss"
	toolPatch   = "patch"
)

func TestFinalStepsExpanded(t *testing.T) {
	t.Parallel()
	h := startBot(t, func(c *telegram.Config) { c.Steps = telegram.StepsExpanded })
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: "read_file", kContext: "a.go"})
	h.tg.wait(mSend, "Running")
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{kToolID: "t1", kName: "read_file"})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Read.", kStatus: statusComplete})
	final := h.tg.wait(mEdit, "Worked for")
	if !strings.Contains(final.text(), "<blockquote>✓") || strings.Contains(final.text(), "expandable") {
		t.Fatalf("final status = %q", final.text())
	}
}

func TestNoStepsButtonWhenStepsOff(t *testing.T) {
	t.Parallel()
	h := startBot(t, func(c *telegram.Config) { c.Steps = telegram.StepsOff })
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{kToolID: "t1", kName: toolTerminal, kContext: "ls"})
	if s := h.tg.wait(mSend, "Running"); len(s.buttons()) != 0 {
		t.Fatalf("buttons with steps off = %v", s.buttons())
	}
}

type fakePins map[string]map[hub.SessionRef]hub.ChatMark

func (f fakePins) ChatMarks(_ context.Context, user string) (map[hub.SessionRef]hub.ChatMark, time.Time, error) {
	return f[user], time.Time{}, nil
}

func TestSessionsListPinsFirstInPinOrder(t *testing.T) {
	t.Parallel()
	pins := fakePins{userAlice: {
		{BackendID: beLaptop, SessionID: "p1"}:  {Pinned: true, PinPos: 0},
		{BackendID: beCluster, SessionID: "p2"}: {Pinned: true, PinPos: -1},
		{BackendID: beLaptop, SessionID: "p3"}:  {Pinned: true, PinPos: 1},
	}}
	h := startBotNotices(t, nil, newFakeState(), nil, nil, func(o *telegram.Options) { o.Pins = pins })
	h.hub.mu.Lock()
	h.hub.sessions = []hub.Session{
		{ID: "r-new", BackendID: beLaptop, Title: "Recent new", UpdatedAt: time.Unix(900, 0)},
		{ID: "p1", BackendID: beLaptop, Title: "Pin one", UpdatedAt: time.Unix(10, 0)},
		{ID: "p2", BackendID: beCluster, Title: "Pin two", UpdatedAt: time.Unix(20, 0)},
		{ID: "r-old", BackendID: beLaptop, Title: "Recent old", UpdatedAt: time.Unix(800, 0)},
		{ID: "p3", BackendID: beLaptop, Title: "Pin three", UpdatedAt: time.Unix(990, 0)},
	}
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/sessions")
	text := h.tg.wait(mSend, "<b>Sessions</b>").text()
	at := make([]int, 0, 5)
	for _, name := range []string{"Pin two", "Pin one", "Pin three", "Recent new", "Recent old"} {
		at = append(at, strings.Index(text, name))
	}
	if !slices.IsSorted(at) || slices.Contains(at, -1) {
		t.Fatalf("session list is not in pin order, then recent: %q", text)
	}
	if strings.Count(text, " pinned") != 3 {
		t.Fatalf("pinned tags = %d in %q", strings.Count(text, " pinned"), text)
	}
}
