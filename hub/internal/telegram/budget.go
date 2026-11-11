package telegram

import (
	"sync"
	"time"
)

// Telegram lets a bot send about one message a second to a private chat
// and about 20 a minute to a group (https://core.telegram.org/bots/faq,
// "My bot is hitting limits"). Edits count as well. All topics of a group
// share the group's limit.
const (
	privateEditGap = time.Second
	groupEditGap   = 3 * time.Second
)

// budget spaces the optional edits of each Telegram chat, such as status
// lines and to-do lists, so that all of a chat's topics together stay
// under Telegram's limits. Messages that must go, such as replies, are
// never held back, but they use up the chat's next slot.
type budget struct {
	mu   sync.Mutex
	now  func() time.Time
	next map[int64]time.Time
	// private and group are the least times between two messages or
	// edits in a private chat and in a group.
	private, group time.Duration
}

func newBudget(now func() time.Time) *budget {
	return &budget{now: now, next: map[int64]time.Time{}, private: privateEditGap, group: groupEditGap}
}

// gap is the least time between two messages or edits in chat.
func (b *budget) gap(chat int64) time.Duration {
	if chat < 0 {
		return b.group
	}
	return b.private
}

// take claims the chat's next slot for an optional edit. It returns 0 when
// the edit may go now, or else how long until the slot is free; then it
// claims nothing.
func (b *budget) take(chat int64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if at := b.next[chat]; now.Before(at) {
		return at.Sub(now)
	}
	b.next[chat] = now.Add(b.gap(chat))
	b.prune(now)
	return 0
}

// spend records a message or edit that went regardless, so the chat's
// next optional edit waits a gap after it. Sends do not pile up a debt:
// an edit waits at most one gap after the last of them.
func (b *budget) spend(chat int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if at := now.Add(b.gap(chat)); at.After(b.next[chat]) {
		b.next[chat] = at
	}
	b.prune(now)
}

// prune forgets the chats whose slot is long free. b.mu is held.
func (b *budget) prune(now time.Time) {
	if len(b.next) < 256 {
		return
	}
	for chat, at := range b.next {
		if now.Sub(at) > time.Minute {
			delete(b.next, chat)
		}
	}
}
