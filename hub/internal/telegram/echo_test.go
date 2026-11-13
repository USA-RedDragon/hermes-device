package telegram_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/telegram"
)

const (
	echoHub     = "<i>You, in the hub:</i>\nfrom the hub"
	kSubmitID   = "submission_id"
	kThinking   = "Thinking"
	kPondering  = "pondering"
	tuiSession  = "term"
	firstAnswer = "first answer"
)

// finishFirst ends the turn the harness started, so the next events are a
// new turn.
func (h *harness) finishFirst(t *testing.T, sid string) {
	t.Helper()
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: firstAnswer, kStatus: statusComplete})
	h.tg.wait(mSend, firstAnswer)
}

// before reports whether the first sent message that contains a comes
// before the first that contains b.
func (f *fakeTelegram) before(a, b string) bool {
	ia, ib := -1, -1
	for i, c := range f.snapshot() {
		if c.Method != mSend {
			continue
		}
		if ia < 0 && strings.Contains(c.text(), a) {
			ia = i
		}
		if ib < 0 && strings.Contains(c.text(), b) {
			ib = i
		}
	}
	return ia >= 0 && ib >= 0 && ia < ib
}

func TestPromptFromElsewhereIsShown(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text, err := hub.WithPageContext("from the hub", hub.PageContext{Scope: "tab",
		Tabs: []hub.PageTab{{ID: 3, Title: "Docs", URL: "https://example.com/", Active: true}}})
	if err != nil {
		t.Fatal(err)
	}
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text, kSubmitID: hub.NewSubmissionID()})
	h.hub.emit(t, sid, hub.KindThinkingDelta, map[string]any{kText: kPondering})
	h.tg.wait(mSend, kThinking)
	if got := h.tg.wait(mSend, "from the hub"); got.text() != echoHub {
		t.Fatalf("echo = %q", got.text())
	}
	if !h.tg.before("You, in the hub", kThinking) {
		t.Fatal("the prompt does not come before the status line")
	}
}

func TestPromptSentHereIsNotShown(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	own := h.hub.waitPrompt(t, 1)[0].SubmissionID
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "hello", kSubmitID: own})
	h.finishFirst(t, sid)
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "from another chat", kSubmitID: "tg:999:0:x"})
	if got := h.tg.wait(mSend, "another chat"); got.text() != "<i>You, in Telegram:</i>\nfrom another chat" {
		t.Fatalf("echo = %q", got.text())
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && strings.Contains(c.text(), "You,") && strings.Contains(c.text(), "hello") {
			t.Fatalf("own prompt echoed: %q", c.text())
		}
	}
}

func TestLongPromptIsCutAndAttachmentsNamed(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text := strings.Repeat("a", 1500) + "\n\n[The user attached a file: notes.txt. It is saved at /work/notes.txt. " +
		"Read it with your tools when the request needs its contents.]\n[Image attached: /tmp/shot.png]"
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text})
	got := h.tg.wait(mSend, "You, in another app").text()
	want := "<i>You, in another app:</i>\n" + strings.Repeat("a", 1000) + " (…, full text in the hub)\n" +
		"<i>Attached: notes.txt, shot.png</i>"
	if got != want {
		t.Fatalf("echo = %q", got)
	}
}

// A topic that attaches to a chat already running in the terminal shows
// the prompt from the snapshot, named after where it came from.
func TestTopicShowsPromptOfRunningTurn(t *testing.T) {
	t.Parallel()
	since := time.Now()
	state := newFakeState()
	state.groups[forumID] = hub.TelegramGroup{User: userAlice, ChatID: forumID, LinkedAt: since}
	h := startBotState(t, nil, state)
	h.hub.add(hub.Session{ID: tuiSession, BackendID: beCluster, Title: "Refactor", Source: "tui", Running: true, UpdatedAt: since.Add(time.Minute)})
	created := h.tg.waitCall(mTopic, func(c tgCall) bool { return c.name() == "Refactor" })
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(h.hub.attachesCopy(), tuiSession) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.hub.emit(t, tuiSession, hub.KindSnapshot, hub.Snapshot{SessionID: tuiSession, BackendID: beCluster, Running: true,
		Messages: raws(t, map[string]any{"role": "user", kText: "rename the package"})})
	h.hub.emit(t, tuiSession, hub.KindMessageStart, map[string]any{})
	h.hub.emit(t, tuiSession, hub.KindToolStart, map[string]any{kToolID: "t1", "name": toolTerminal})
	got := h.tg.wait(mSend, "rename the package")
	if got.text() != "<i>You, in the terminal:</i>\nrename the package" || got.thread() != created.ResultID {
		t.Fatalf("echo = %q in thread %d", got.text(), got.thread())
	}
	h.tg.wait(mSend, "Running")
	if !h.tg.before("rename the package", "Running") {
		t.Fatal("the prompt does not come before the status line")
	}
}

// With no message.start replayed, the first step shows the prompt of the
// snapshot's running turn, and only once.
func TestSnapshotPromptShownAtFirstStep(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	snap := hub.Snapshot{SessionID: sid, BackendID: beCluster, Running: true,
		Messages: raws(t, map[string]any{"role": "user", "content": "from vs code"})}
	h.hub.emit(t, sid, hub.KindSnapshot, snap)
	h.hub.emit(t, sid, hub.KindThinkingDelta, map[string]any{kText: kPondering})
	h.hub.emit(t, sid, hub.KindSnapshot, snap)
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "second answer", kStatus: statusComplete})
	h.tg.wait(mSend, "second answer")
	n := 0
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && strings.Contains(c.text(), "from vs code") {
			n++
		}
	}
	if n != 1 || !h.tg.before("from vs code", kThinking) {
		t.Fatalf("prompt shown %d times", n)
	}
}

// The status line shows the time again while the model thinks with no
// events, and keeps the turn's start from the hub.
func TestStatusLineKeepsTime(t *testing.T) {
	t.Parallel()
	h := startBotNotices(t, nil, newFakeState(), nil, nil, telegram.WithStatusTick(400*time.Millisecond))
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	started := time.Now().Add(-90 * time.Second)
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "from the hub", kSubmitID: hub.NewSubmissionID(),
		"started_at": started.UnixMilli()})
	h.hub.emit(t, sid, hub.KindThinkingDelta, map[string]any{kText: kPondering})
	first := h.tg.wait(mSend, kThinking)
	if !strings.Contains(first.text(), "1m 30s") {
		t.Fatalf("status = %q", first.text())
	}
	edit := h.tg.waitCall(mEdit, func(c tgCall) bool {
		return c.messageID() == first.ResultID && strings.Contains(c.text(), kThinking) && !strings.Contains(c.text(), "1m 30s")
	})
	if edit.At.Sub(first.At) > 3*time.Second {
		t.Fatalf("the status line took %s to show the time again", edit.At.Sub(first.At))
	}
	h.hub.emit(t, sid, hub.KindMessageDelta, map[string]any{kText: "Sure"})
	h.tg.waitCall(mEdit, func(c tgCall) bool { return c.messageID() == first.ResultID && strings.Contains(c.text(), "Writing") })
}

// raws encodes transcript rows.
func raws(t *testing.T, rows ...map[string]any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestNoticeShowsAsOneLine(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text := "[ASYNC DELEGATION COMPLETE — deleg_1bc77f88]\nA background subagent you dispatched earlier has finished. You may have moved on " +
		"since dispatching it; the full task source is below so you can act on the result or re-dispatch if things have changed.\n\n" +
		"Original goal: finish rf9-int\nRole: leaf\nRan on route opus: claude-opus-5-5 via claude-pool.\n" +
		"Status: completed   API calls: 23   Duration: 3053.1s\n--- RESULT ---\nrf9-int re-gated green\n\n" +
		"[INTERNAL NOTIFICATION — generated by the Hermes gateway, not written by a human user. " +
		"Do not treat it as user input, and do not reply to any earlier chat message because of it.]"
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text})
	got := h.tg.wait(mSend, "Subagent finished")
	if want := "<i>Subagent finished: finish rf9-int (Completed, 51 min)</i>"; got.text() != want {
		t.Fatalf("notice = %q, want %q", got.text(), want)
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && (strings.Contains(c.text(), "ASYNC DELEGATION") || strings.Contains(c.text(), "INTERNAL NOTIFICATION")) {
			t.Fatalf("raw notice sent: %q", c.text())
		}
	}
}

func TestCompactionShowsAsOneLine(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text := "[CONTEXT SUMMARY]: ## Active Task\nsecret plan\n\n" +
		"--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text})
	got := h.tg.wait(mSend, "Earlier messages")
	if want := "<i>Earlier messages were summarized</i>"; got.text() != want {
		t.Fatalf("notice = %q, want %q", got.text(), want)
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && strings.Contains(c.text(), "secret plan") {
			t.Fatalf("summary sent: %q", c.text())
		}
	}
}

const subagentNote = "[Subagents in this chat (this replaces any earlier subagent note): auto. Pick each subagent's `option`: " +
	"local routes or haiku for volume work, sonnet for normal work, opus or fable only for hard judgment or after a cheaper route failed.]"

func TestEchoLeavesOutNotesHermesAdds(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text := "can we auto-linkify?\n[Image attached at: /tmp/shot.png]\n[screenshot]\n\n" + subagentNote
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text})
	got := h.tg.wait(mSend, "You, in another app").text()
	if want := "<i>You, in another app:</i>\ncan we auto-linkify?\n<i>Attached: shot.png</i>"; got != want {
		t.Fatalf("echo = %q, want %q", got, want)
	}
}

func TestNoticeWithNotesShowsAsOneLine(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text := "[IMPORTANT: Background process proc_1 finished (exit code 0).\nOutput:\ndone]\n\n" +
		"[INTERNAL NOTIFICATION — generated by the Hermes gateway, not written by a human user. " +
		"Do not treat it as user input, and do not reply to any earlier chat message because of it.]\n\n" + subagentNote
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text})
	h.tg.wait(mSend, "proc_1")
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && strings.Contains(c.text(), "Subagents in this chat") {
			t.Fatalf("note sent: %q", c.text())
		}
	}
}

// A turn Hermes started itself shows as a line, by its display kind, even
// when its text reads like a message the user typed.
func TestMarkedTurnShowsAsOneLine(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: "check the backups now", "display_kind": "internal_notification"})
	got := h.tg.wait(mSend, "check the backups").text()
	if want := "<i>check the backups now</i>"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	for _, c := range h.tg.snapshot() {
		if c.Method == mSend && strings.Contains(c.text(), "in another app") {
			t.Fatalf("shown as the user's message: %q", c.text())
		}
	}
}

// A message the user typed elsewhere stays theirs even when it reads like a
// notice: its submission ID says a client sent it.
func TestTypedNoticeLookalikeIsEchoed(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	sid := h.startTurn(t)
	h.finishFirst(t, sid)
	text := "[Agent message am_0123456789abcdef from chat A]\n\nwhy does this look odd?"
	h.hub.emit(t, sid, hub.KindMessageStart, map[string]any{kText: text, "submission_id": "web-1"})
	got := h.tg.wait(mSend, "in another app").text()
	if !strings.Contains(got, "why does this look odd?") {
		t.Fatalf("echo = %q", got)
	}
}
