package telegram_test

import (
	"context"
	"html"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

const (
	promptLong   = "long"
	scopePrivate = "all_private_chats"
)

type fakePrompts struct {
	mu    sync.Mutex
	users map[string][]hub.SavedPrompt
}

func (f *fakePrompts) Prompts(_ context.Context, user string) ([]hub.SavedPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.users[user]), nil
}

func (f *fakePrompts) Prompt(_ context.Context, user, name string) (hub.SavedPrompt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.users[user] {
		if hub.PromptKey(p.Name) == hub.PromptKey(name) {
			return p, true, nil
		}
	}
	return hub.SavedPrompt{}, false, nil
}

func (f *fakePrompts) remove(user, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[user] = slices.DeleteFunc(f.users[user], func(p hub.SavedPrompt) bool { return p.Name == name })
}

// unpre takes the text back out of a message with a code block.
func unpre(t *testing.T, msg string) string {
	t.Helper()
	_, rest, ok := strings.Cut(msg, "<pre><code>")
	body, _, ok2 := strings.Cut(rest, "</code></pre>")
	if !ok || !ok2 {
		t.Fatalf("no code block in %q", msg)
	}
	return html.UnescapeString(body)
}

func TestPromptsCommand(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	for i := range 400 {
		sb.WriteString(strings.Repeat("<tag> & step ", 2) + string(rune('a'+i%26)) + "\n")
	}
	longText := strings.TrimSuffix(sb.String(), "\n")
	lib := &fakePrompts{users: map[string][]hub.SavedPrompt{
		userAlice: {{Name: promptLong, Text: longText}, {Name: "review", Text: "Review <this> & that.\n\n- one\n- two"}},
	}}
	h := startBotNotices(t, nil, newFakeState(), nil, nil, func(o *telegram.Options) { o.Prompts = lib })

	menu := h.tg.waitCall(mCommands, func(c tgCall) bool { typ, _ := scopeOf(c); return typ == scopePrivate })
	if !slices.Contains(commandNames(menu), "prompts") {
		t.Fatalf("private menu %q lacks prompts", commandNames(menu))
	}

	h.tg.say(dm(aliceTG), aliceTG, "/prompts")
	list := h.tg.wait(mSend, "<b>Prompts</b>")
	btns := list.buttons()
	if len(btns) != 2 || btns[0][kText] != promptLong || btns[1][kText] != "review" {
		t.Fatalf("buttons = %+v", btns)
	}
	if !strings.Contains(list.text(), "Settings &gt; Prompts") {
		t.Fatalf("list = %q", list.text())
	}
	short, _ := btns[1]["callback_data"].(string)
	long, _ := btns[0]["callback_data"].(string)

	h.tg.press(dm(aliceTG), aliceTG, list.ResultID, short)
	got := h.tg.wait(mSend, "<b>review</b>\n<pre><code>")
	if text := unpre(t, got.text()); text != "Review <this> & that.\n\n- one\n- two" {
		t.Fatalf("prompt text = %q", text)
	}

	h.tg.press(dm(aliceTG), aliceTG, list.ResultID, long)
	first := h.tg.wait(mSend, "<b>"+promptLong+"</b>\n<pre><code>")
	n := telegram.PromptChunks(longText)
	if n < 3 {
		t.Fatalf("the long prompt takes %d messages, want at least 3", n)
	}
	parts := make([]string, 0, n)
	parts = append(parts, unpre(t, first.text()))
	more := h.tg.waitN(mSend, "<b>"+promptLong+"</b> (continued)", n-1)
	for _, c := range more {
		parts = append(parts, unpre(t, c.text()))
	}
	for _, c := range append([]tgCall{first}, more...) {
		if n := len(utf16.Encode([]rune(c.text()))); n > telegram.MaxMessageLen {
			t.Fatalf("a message of %d UTF-16 units is over the limit", n)
		}
	}
	if strings.Join(parts, "\n") != longText {
		t.Fatalf("the parts do not add up to the prompt: %d parts", len(parts))
	}

	lib.remove(userAlice, "review")
	h.tg.press(dm(aliceTG), aliceTG, list.ResultID, short)
	h.tg.wait(mAnswer, "That prompt no longer exists")

	h.tg.say(dm(aliceTG), aliceTG, "/prompts")
	h.tg.waitN(mSend, "<b>Prompts</b>", 2)
	h.tg.press(dm(aliceTG), aliceTG, list.ResultID, long)
	h.tg.wait(mAnswer, "That list is out of date")

	h.tg.say(dm(bobTG), bobTG, "/prompts")
	h.tg.wait(mSend, "You have no prompts yet.")
}

func TestPromptsCommandOff(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.say(dm(aliceTG), aliceTG, "/prompts")
	h.tg.wait(mSend, "This hub keeps no prompts.")
	menu := h.tg.waitCall(mCommands, func(c tgCall) bool { typ, _ := scopeOf(c); return typ == scopePrivate })
	if slices.Contains(commandNames(menu), "prompts") {
		t.Fatalf("private menu %q has prompts without a library", commandNames(menu))
	}
}
