package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// meEvery is how often the bot reads getMe again, to learn when topics in
// private chats are turned on or off in @BotFather.
const meEvery = time.Minute

// refreshMe reads getMe when the last read is older than meEvery, and
// returns what the bot knows of itself, or nil before a read worked.
func (b *Bot) refreshMe(ctx context.Context) *Me {
	if at := b.meAt.Load(); at != 0 && b.now().Sub(time.Unix(0, at)) < meEvery {
		return b.me.Load()
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	me, err := b.api.GetMe(cctx)
	b.meAt.Store(b.now().UnixNano())
	if err != nil {
		b.logger.Warn("Failed to read the bot's own user", "error", err)
		return b.me.Load()
	}
	old := b.me.Swap(&me)
	if old == nil || old.HasTopicsEnabled != me.HasTopicsEnabled {
		b.logger.Info("Topics in private chats", "enabled", me.HasTopicsEnabled)
	}
	return &me
}

// dmTopics reports whether the bot has topics in private chats, as getMe
// last said (Bot API 9.3, turned on in @BotFather).
func (b *Bot) dmTopics() bool {
	me := b.me.Load()
	return me != nil && me.HasTopicsEnabled
}

// isDM reports whether a chat ID is a private chat's.
func isDM(chatID int64) bool { return chatID > 0 }

// generalName names where a forum's notices go and new chats start: a
// group's General topic, or the main thread of a private chat.
func generalName(chatID int64) string {
	if isDM(chatID) {
		return "the main chat"
	}
	return "General"
}

// topicURL opens a topic in Telegram: in a supergroup, or (Bot API 10.4)
// in the private chat with the bot, where it opens only for its user. It
// is "" when the bot cannot make one.
func (b *Bot) topicURL(ctx context.Context, chatID, thread int64) string {
	if thread == 0 {
		return ""
	}
	if !isDM(chatID) {
		return topicURL(chatID, thread)
	}
	me := b.me.Load()
	if me == nil {
		me = b.refreshMe(ctx)
	}
	if me == nil || me.Username == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/%s/%d", me.Username, thread)
}

// hasForum reports whether user has a linked group or a private chat with
// topics. b.fmu is not held.
func (b *Bot) hasForum(user string) bool {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	for _, f := range b.forums {
		if f.user == user {
			return true
		}
	}
	return false
}

// dmForum turns on topics in the private chat chatID of user, who has no
// linked group: each of the user's chats gets a topic there, as in a
// linked group. It returns nil when it cannot. b.loop is held.
func (b *Bot) dmForum(ctx context.Context, chatID int64, user string) *forum {
	if b.state == nil || !b.dmTopics() || b.hasForum(user) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	// The store decides: linking replaces the user's group, so a group
	// that the bot failed to load must not be lost here.
	groups, err := b.state.TelegramGroups(cctx)
	if err != nil {
		b.logger.Warn("Failed to read the linked groups, keeping the private chat without topics", "error", err)
		return nil
	}
	for _, g := range groups {
		if g.User == user {
			return nil
		}
	}
	g := hub.TelegramGroup{User: user, ChatID: chatID, LinkedAt: b.now()}
	if err := b.state.LinkTelegramGroup(cctx, g); err != nil {
		b.logger.Error("Failed to turn on topics in a private chat", "chat", chatID, "error", err)
		return nil
	}
	b.dropForum(chatID)
	f := newForum(g)
	b.fmu.Lock()
	b.forums[chatID] = f
	b.fmu.Unlock()
	b.logger.Info("Turned on topics in a private chat", "chat", chatID, "user", user)
	b.sendText(ctx, dest{chat: chatID}, "Each of your chats now gets its own topic here, named after the chat. "+
		"A message here in the main chat starts a new chat in a new topic. Notices come here.")
	return f
}

// dmOwner reports whether the Telegram account from is still linked to
// the user of the private chat f. When it is not, the chat is that
// account's own again, and the bot must not route the old user's chats
// and notices there. An error to look it up counts as linked. b.loop is
// held.
func (b *Bot) dmOwner(ctx context.Context, f *forum, from int64) bool {
	user, linked, err := b.lookupUser(ctx, from)
	return err != nil || linked && user == f.user
}

// dropDMs turns topics off in every private chat after @BotFather turned
// them off for the bot. b.loop is not held.
func (b *Bot) dropDMs(ctx context.Context) {
	b.fmu.Lock()
	var dms []*forum
	for _, f := range b.forums {
		if isDM(f.chatID) {
			dms = append(dms, f)
		}
	}
	b.fmu.Unlock()
	if len(dms) == 0 {
		return
	}
	b.loop.Lock()
	defer b.loop.Unlock()
	for _, f := range dms {
		if b.forum(f.chatID) == f {
			b.dropDM(ctx, f)
		}
	}
}

// dropDM turns topics off in a private chat after they were turned off in
// @BotFather: the chat follows one hub chat at a time again. The topics
// stay in Telegram. b.loop is held.
func (b *Bot) dropDM(ctx context.Context, f *forum) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.state.UnlinkTelegramGroup(cctx, f.chatID); err != nil {
		b.logger.Error("Failed to turn off topics in a private chat", "chat", f.chatID, "error", err)
		return
	}
	b.dropForum(f.chatID)
	b.logger.Info("Turned off topics in a private chat", "chat", f.chatID, "user", f.user)
}

// unlinkFromDM handles /unlink in a private chat: it unlinks the user's
// group, so their chats get topics in the private chat when the bot has
// them. b.loop is held.
func (b *Bot) unlinkFromDM(ctx context.Context, to dest, user string) {
	var group *forum
	b.fmu.Lock()
	for _, f := range b.forums {
		if f.user == user && !isDM(f.chatID) {
			group = f
		}
	}
	b.fmu.Unlock()
	if group == nil || b.state == nil {
		b.sendText(ctx, to, "You have no linked group.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.state.UnlinkTelegramGroup(cctx, group.chatID); err != nil {
		b.logger.Error("Failed to unlink a Telegram group", "chat", group.chatID, "error", err)
		b.sendText(ctx, to, errGeneric)
		return
	}
	b.dropForum(group.chatID)
	b.groupMenu(ctx, group.chatID, false)
	b.logger.Info("Unlinked a Telegram group from a private chat", "chat", group.chatID, "user", user)
	msg := "Unlinked your group. Its topics stay, but chats no longer get topics there."
	if b.dmTopics() {
		msg += " Send a message here to give each chat its own topic in this chat."
	}
	b.sendText(ctx, to, msg)
}

// archivedSuffix marks the topic of an archived chat in a private chat,
// where Telegram cannot close topics (closeForumTopic is for supergroups).
const archivedSuffix = " · archived"

// archivedName is the name of an archived chat's topic in a private chat.
func archivedName(name string) string {
	if strings.HasSuffix(name, archivedSuffix) {
		return name
	}
	return truncate(name, maxTopicName-len([]rune(archivedSuffix))) + archivedSuffix
}
