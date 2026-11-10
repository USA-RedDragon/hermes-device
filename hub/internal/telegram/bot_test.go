package telegram_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
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
	h.tg.say(telegram.Chat{ID: -999, Type: "supergroup"}, 77, "ignored group")
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
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "done", kStatus: "complete"})
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
	if !strings.Contains(last, "Thought for") || !strings.Contains(last, "<blockquote expandable>") ||
		!strings.Contains(last, "LAST") || !strings.Contains(last, "&lt;x&gt;") {
		t.Fatalf("final thinking edit = %q", last)
	}
}

func TestToolStartAndComplete(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindToolStart, map[string]any{"tool_id": "t1", "name": toolTerminal, "context": "ls -la /srv"})
	start := h.tg.wait(mSend, toolTerminal)
	if start.text() != "▶ <b>terminal</b> <code>ls -la /srv</code>" {
		t.Fatalf("tool start = %q", start.text())
	}
	h.hub.emit(t, sid, hub.KindToolComplete, map[string]any{"tool_id": "t1", "name": toolTerminal, "duration_s": 1.234, "summary": "3 files"})
	done := h.tg.wait(mEdit, "✓")
	if done.messageID() != start.ResultID {
		t.Fatalf("complete edited message %d, start was %d", done.messageID(), start.ResultID)
	}
	want := "✓ <b>terminal</b> <code>ls -la /srv</code> <i>(1.2s)</i>\n3 files"
	if done.text() != want {
		t.Fatalf("tool complete = %q, want %q", done.text(), want)
	}
}

func TestFinalAnswerIsSplit(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	long := strings.Repeat("word & <tag> ", 900)
	h.hub.emit(t, sid, hub.KindMessageDelta, map[string]any{kText: "partial"})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: long, kStatus: "complete"})
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
	req := h.tg.wait(mSend, "Approval needed")
	btns := req.buttons()
	if len(btns) != 2 || btns[0][kText] != "Allow" || btns[1][kText] != "Deny" {
		t.Fatalf("buttons = %+v", btns)
	}
	msgID := req.ResultID
	allow, _ := btns[0]["callback_data"].(string)
	h.tg.press(dm(aliceTG), aliceTG, msgID, allow)
	edit := h.tg.wait(mEdit, "Allowed by alice")
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
	req := h.tg.wait(mSend, "Approval needed")
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
		{ID: "old", BackendID: beLaptop, Title: "Old", UpdatedAt: time.Unix(100, 0)},
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
	if p[0].SessionID != "old" || p[0].BackendID != beLaptop {
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
	h.hub.sessions = []hub.Session{{ID: "old", BackendID: beLaptop, Title: "Old", UpdatedAt: time.Unix(100, 0)}}
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
