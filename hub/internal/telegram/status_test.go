package telegram_test

import (
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func TestStatusShowsContext(t *testing.T) {
	t.Parallel()
	const kUsage, kUsed, kMax, kModel, window = "usage", "context_used", "context_max", "model", 131072
	h := startBot(t, nil)
	sid := h.startTurn(t)
	turn := func(text string) {
		h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: text, kStatus: statusComplete})
		h.tg.wait(mSend, text)
		h.tg.say(dm(aliceTG), aliceTG, "/status")
	}
	check := func(contains, want string) {
		if got := h.tg.wait(mSend, contains).text(); got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	}

	h.hub.emit(t, sid, hub.KindSnapshot, hub.Snapshot{SessionID: sid, Info: hub.Info{Model: "qwen-local", ContextLength: window}})
	turn("first")
	check("131k window", "Model: <b>qwen-local</b>\nContext: 131k window")

	h.hub.emit(t, sid, "session.usage", map[string]any{kUsage: map[string]any{kUsed: 41000, kMax: window}})
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "second", kStatus: statusComplete,
		kUsage: map[string]any{kUsed: 112000, kMax: window}})
	h.tg.wait(mSend, "second")
	h.tg.say(dm(aliceTG), aliceTG, "/status")
	check("112k", "Model: <b>qwen-local</b>\nContext: 112k / 131k (85%). Nearly full: Talaria will summarize older messages soon.")

	h.hub.emit(t, sid, "session.updated", map[string]any{"operation": kModel, kModel: "claude-api"})
	turn("third")
	check("next reply", "Model: <b>claude-api</b>\nContext: shows after the next reply")

	h.hub.emit(t, sid, "session.info", map[string]any{kModel: "claude-api", "context_length": 1000000,
		kUsage: map[string]any{kUsed: 2400, kMax: 1000000}})
	turn("fourth")
	check("1M", "Model: <b>claude-api</b>\nContext: 2k / 1M (0%)")
}

func TestStatusNeedsAttachment(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.say(dm(aliceTG), aliceTG, "/status")
	h.tg.wait(mSend, "No chat is attached")
}
