package telegram

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// groupEditInterval is the least time between edits of a status line in
// a topic: all topics of a group share the group's rate limit.
const groupEditInterval = 3 * time.Second

// topicIdle is how long a topic's stream stays attached after its chat
// stopped running and the topic shows no prompt.
const topicIdle = 15 * time.Minute

// forum is a linked group: a forum supergroup where each chat of its user
// has a topic.
type forum struct {
	chatID   int64
	user     string
	linkedAt time.Time
	// topics are the topics by chat, threads the chats by topic. b.fmu
	// guards both.
	topics  map[hub.SessionRef]hub.TelegramTopic
	threads map[int64]hub.SessionRef
	// reconciled is when the bot last checked that the chats of the
	// topics still exist, and alive the chats it found, by when. b.loop
	// guards both.
	reconciled time.Time
	alive      map[hub.SessionRef]time.Time
	// toldRights is set once the bot said in General that it lacks the
	// rights to close or delete topics. b.loop guards it.
	toldRights bool
}

func newForum(g hub.TelegramGroup) *forum {
	return &forum{chatID: g.ChatID, user: g.User, linkedAt: g.LinkedAt,
		topics: map[hub.SessionRef]hub.TelegramTopic{}, threads: map[int64]hub.SessionRef{}, alive: map[hub.SessionRef]time.Time{}}
}

// forum is the linked group with this chat ID, or nil.
func (b *Bot) forum(chatID int64) *forum {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	return b.forums[chatID]
}

// topic is the topic of a chat in f.
func (b *Bot) topic(f *forum, ref hub.SessionRef) (hub.TelegramTopic, bool) {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	t, ok := f.topics[ref]
	return t, ok
}

// threadRef is the chat of a topic in f.
func (b *Bot) threadRef(f *forum, thread int64) (hub.SessionRef, bool) {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	ref, ok := f.threads[thread]
	return ref, ok
}

// restoreForums loads the linked groups and their topics.
func (b *Bot) restoreForums(ctx context.Context) {
	if b.state == nil {
		return
	}
	groups, err := b.state.TelegramGroups(ctx)
	if err != nil {
		b.logger.Error("Failed to load Telegram groups", "error", err)
		return
	}
	forums := make(map[int64]*forum, len(groups))
	for _, g := range groups {
		f := newForum(g)
		topics, err := b.state.TelegramTopics(ctx, g.ChatID)
		if err != nil {
			b.logger.Error("Failed to load Telegram topics", "chat", g.ChatID, "error", err)
		}
		for _, t := range topics {
			ref := hub.SessionRef{BackendID: t.BackendID, SessionID: t.SessionID}
			f.topics[ref] = t
			if t.ThreadID != 0 {
				f.threads[t.ThreadID] = ref
			}
		}
		forums[g.ChatID] = f
	}
	b.fmu.Lock()
	b.forums = forums
	b.fmu.Unlock()
	if len(forums) > 0 {
		b.logger.Info("Restored Telegram groups", "groups", len(forums))
	}
}

// topicURL opens a topic of a supergroup in Telegram, or is "" when chatID
// is not a supergroup's.
func topicURL(chatID, thread int64) string {
	internal := -chatID - 1_000_000_000_000
	if internal <= 0 || thread == 0 {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", internal, thread)
}

// topicLink opens the topic of a chat in the linked group or private chat
// chatID, or is "" when it has none.
func (b *Bot) topicLink(ctx context.Context, chatID int64, backendID, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if f := b.forum(chatID); f != nil {
		t, _ := b.topic(f, hub.SessionRef{BackendID: backendID, SessionID: sessionID})
		return b.topicURL(ctx, chatID, t.ThreadID)
	}
	if b.state == nil || chatID > -1_000_000_000_000 && !isDM(chatID) {
		return ""
	}
	// A replica that does not run the bot sends notices too: it reads the
	// topics from the store.
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	topics, err := b.state.TelegramTopics(cctx, chatID)
	if err != nil {
		return ""
	}
	for _, t := range topics {
		if t.BackendID == backendID && t.SessionID == sessionID {
			return b.topicURL(ctx, chatID, t.ThreadID)
		}
	}
	return ""
}

// topicName is the name of a chat's topic: its title, and where it runs
// when that is a computer or the sandbox.
func topicName(title, place string) string {
	name := strings.Join(strings.Fields(title), " ")
	if name == "" {
		name = "New chat"
	}
	if place != "" {
		name = truncate(name, maxTopicName-len([]rune(place))-3) + " · " + place
	}
	return truncate(name, maxTopicName)
}

// sourceCron is the source of the sessions of Hermes's scheduled jobs.
const sourceCron = "cron"

// topicSource reports whether chats of a source get topics: the user's own
// chats, including those of a computer's talaria daemon ("local"), and
// scheduled jobs, not the conversations that Hermes holds on a
// messaging platform, which may have other people in them, nor its
// subagents. "" is a backend that does not say.
func topicSource(source string) bool {
	switch source {
	case "", "gui", "cli", "tui", "acp", "desktop", "local", sourceCron:
		return true
	}
	return false
}

// placeName names where a chat runs, for its topic: a computer's name, or
// the sandbox.
func placeName(p hub.Place, ok bool) string {
	switch {
	case !ok:
		return ""
	case p.Kind == hub.PlaceComputer:
		return p.Name
	case p.Kind == hub.PlaceSandbox:
		return "sandbox"
	}
	return ""
}

// linkGroup links a forum group to the hub user of the Telegram account
// that sent /link there. b.loop is held.
func (b *Bot) linkGroup(ctx context.Context, m *Message) {
	to := dest{chat: m.Chat.ID}
	if b.state == nil {
		b.sendText(ctx, to, "This hub cannot link groups: it keeps no state.")
		return
	}
	user, linked, err := b.lookupUser(ctx, m.From.ID)
	switch {
	case err != nil:
		b.sendText(ctx, to, errGeneric)
		return
	case !linked:
		b.sendText(ctx, to, "Link your Telegram account first: send me a direct message.")
		return
	case m.Chat.Type != chatSuper || !m.Chat.IsForum:
		b.sendText(ctx, to, "Turn on Topics for this group first (group settings, Topics), then send /link again.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	member, err := b.api.GetChatMember(cctx, m.Chat.ID, b.botID)
	if err != nil || member.Status != "administrator" || !member.CanManageTopics {
		b.sendText(ctx, to, "Make me an admin of this group with the rights to manage topics and to delete messages, then send /link again.")
		return
	}
	g := hub.TelegramGroup{User: user, ChatID: m.Chat.ID, LinkedAt: b.now()}
	if err := b.state.LinkTelegramGroup(cctx, g); err != nil {
		b.logger.Error("Failed to link a Telegram group", "chat", m.Chat.ID, "error", err)
		b.sendText(ctx, to, errGeneric)
		return
	}
	b.fmu.Lock()
	var old []int64
	for id, f := range b.forums {
		if f.user == user || id == g.ChatID {
			old = append(old, id)
		}
	}
	b.fmu.Unlock()
	for _, id := range append(old, g.ChatID) {
		b.dropForum(id)
		if id != g.ChatID && !isDM(id) {
			b.groupMenu(ctx, id, false)
		}
	}
	b.fmu.Lock()
	b.forums[g.ChatID] = newForum(g)
	b.fmu.Unlock()
	b.groupMenu(ctx, g.ChatID, true)
	b.logger.Info("Linked a Telegram group", "chat", g.ChatID, "user", user)
	b.sendHTML(ctx, to, "Linked this group to <b>"+escapeHTML(user)+"</b>. Each chat that you start or use from now on gets "+
		"a topic here, named after the chat. Send a message here in General to start a new chat. "+
		"Notices and scheduled job results come to General. /unlink undoes this.", nil)
}

// dropForum forgets a linked group and detaches its topics. b.loop is
// held.
func (b *Bot) dropForum(chatID int64) {
	for to, st := range b.chats {
		if to.chat != chatID {
			continue
		}
		if st.att != nil {
			st.att.stop()
		}
		delete(b.chats, to)
	}
	b.fmu.Lock()
	delete(b.forums, chatID)
	b.fmu.Unlock()
}

// handleForum handles a message in a linked group. Only the Telegram
// accounts of the group's user count; the bot ignores everyone else.
// b.loop is held.
func (b *Bot) handleForum(ctx context.Context, f *forum, ms []*Message, media bool) {
	m := ms[0]
	to := dest{chat: m.Chat.ID}
	if m.IsTopicMessage {
		to.thread = m.MessageThreadID
	}
	user, linked, err := b.lookupUser(ctx, m.From.ID)
	if err != nil || !linked || user != f.user {
		return
	}
	if !media && (b.claudeReply(ctx, to, user, m) || b.replyAnswer(ctx, to, user, m)) {
		return
	}
	if r := m.ReplyToMessage; to.thread == 0 && r != nil && r.From != nil && r.From.ID == b.botID && r.ForumTopicCreated == nil {
		b.sendText(ctx, to, "That message takes no answer. To start a new chat, send a message here without replying.")
		return
	}
	cmd, arg := "", ""
	if !media {
		cmd, arg = parseCommand(m.Text)
	}
	b.logger.Debug("Telegram group message", "user", user, "chat", to.chat, "thread", to.thread, "command", cmd)
	dm := isDM(f.chatID)
	switch cmd {
	case cmdLink:
		if dm {
			b.sendText(ctx, to, "Each chat has its own topic here already. To use a group with Topics instead, send /link there.")
			return
		}
		b.sendText(ctx, to, "This group is linked to you already.")
	case cmdUnlink:
		if dm {
			b.unlinkFromDM(ctx, to, user)
			return
		}
		b.unlinkGroup(ctx, f, to)
	case "start", cmdHelp:
		b.forumHelp(ctx, to, user, dm)
	case cmdNew:
		b.forumNew(ctx, f, to, user, arg, nil)
	case cmdSessions, "attach":
		where := "In this group"
		if dm {
			where = "Here"
		}
		b.sendText(ctx, to, where+" each chat has its own topic. Send /new or a message in "+generalName(f.chatID)+" to start one.")
	case cmdBackends:
		b.listBackends(ctx, to, user)
	case cmdDevices, "device", "computers":
		b.listComputers(ctx, to, user)
	case "":
		if to.thread == 0 {
			b.forumNew(ctx, f, to, user, "", ms)
			return
		}
		if !b.ensureTopic(ctx, f, to, user, m) {
			return
		}
		if media {
			b.promptMedia(ctx, to, user, ms)
			return
		}
		b.prompt(ctx, to, user, m.MessageID, m.Text)
	default:
		if to.thread == 0 {
			b.sendText(ctx, to, "Send this in the topic of a chat.")
			return
		}
		if b.ensureTopic(ctx, f, to, user, m) {
			b.sessionCommand(ctx, to, user, cmd, arg)
		}
	}
}

func (b *Bot) forumHelp(ctx context.Context, to dest, user string, dm bool) {
	var sb strings.Builder
	if dm {
		fmt.Fprintf(&sb, "Signed in as <b>%s</b>. Each chat has its own topic here.\n\n", escapeHTML(user))
		sb.WriteString("A message in the main chat starts a new chat in a new topic. ")
	} else {
		fmt.Fprintf(&sb, "This group belongs to <b>%s</b>. Each chat has its own topic.\n\n", escapeHTML(user))
		sb.WriteString("A message in General starts a new chat in a new topic. ")
	}
	sb.WriteString("A message in a topic goes to its chat. " +
		"Reply to a question's message to answer it.\n" +
		"/new [backend] start a new chat\n" +
		"/stop interrupt the turn of this topic's chat\n" +
		"/status show the model and how full its context is\n" +
		"/model show or switch the model of this topic's chat\n" +
		"/bg TEXT run a prompt in the background\n" +
		"/backends list your backends")
	if dm {
		sb.WriteString("\n/unlink stop using your linked group")
	} else {
		sb.WriteString("\n/unlink stop using this group")
	}
	if b.computers != nil {
		sb.WriteString("\n/devices list your computers\n" +
			"/new NAME [folder] start a chat on a computer\n" +
			"/where show where this topic's chat runs\n" +
			"/move NAME [folder] run it on a computer\n" +
			"/move off stop using its computer")
	}
	b.sendHTML(ctx, to, sb.String(), nil)
}

func (b *Bot) unlinkGroup(ctx context.Context, f *forum, to dest) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.state.UnlinkTelegramGroup(cctx, f.chatID); err != nil {
		b.logger.Error("Failed to unlink a Telegram group", "chat", f.chatID, "error", err)
		b.sendText(ctx, to, errGeneric)
		return
	}
	b.dropForum(f.chatID)
	b.groupMenu(ctx, f.chatID, false)
	b.logger.Info("Unlinked a Telegram group", "chat", f.chatID, "user", f.user)
	b.sendText(ctx, to, "Unlinked. Chats no longer get topics here. Your private chat with me still works.")
}

// forumNew starts a chat in a new topic: for /new, or for the message ms
// that someone sent in General, which becomes its first message.
func (b *Bot) forumNew(ctx context.Context, f *forum, general dest, user, arg string, ms []*Message) {
	var (
		s     hub.Session
		place string
	)
	if arg != "" && !b.isBackend(ctx, user, arg) {
		sess, p, matched, ok := b.startOnComputer(ctx, general, user, arg)
		if matched && !ok {
			return
		}
		if matched {
			s, place = sess, p.Name
		}
	}
	if s.ID == "" {
		backendID, ok := b.pickBackend(ctx, general, user, arg)
		if !ok {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		var err error
		s, err = b.svc.NewSession(cctx, user, backendID)
		cancel()
		if err != nil {
			b.reportErr(ctx, general, user, "start session", err)
			return
		}
	}
	text := ""
	if len(ms) > 0 {
		text = ms[0].Text
		if text == "" {
			text = ms[0].Caption
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	name := topicName(truncate(first, 60), place)
	thread, err := b.openTopic(ctx, f, s, name)
	if err != nil {
		b.logger.Error("Failed to create a topic", "chat", f.chatID, "error", err)
		b.sendText(ctx, general, "Could not create a topic for the new chat. Check that I may manage topics here.")
		return
	}
	to := dest{chat: f.chatID, thread: thread}
	if err := b.attach(ctx, to, user, s.BackendID, s.ID, 0); err != nil {
		b.reportErr(ctx, to, user, "attach", err)
		return
	}
	link := &InlineKeyboard{InlineKeyboard: [][]InlineButton{{{Text: "Open the topic", URL: b.topicURL(ctx, f.chatID, thread)}}}}
	if link.InlineKeyboard[0][0].URL == "" {
		link = nil
	}
	if len(ms) == 0 {
		b.sendHTML(ctx, to, "New chat on <b>"+escapeHTML(s.BackendID)+"</b>. Send a message to begin.", nil)
		b.sendHTML(ctx, general, "Started a new chat: <b>"+escapeHTML(name)+"</b>.", link)
		return
	}
	b.sendHTML(ctx, to, "<i>From "+generalName(f.chatID)+":</i>\n"+escapeHTML(truncate(text, 3000)), nil)
	b.sendHTML(ctx, general, "Started a new chat for that: <b>"+escapeHTML(name)+"</b>.", link)
	if slices.ContainsFunc(ms, hasMedia) {
		b.promptMedia(ctx, to, user, ms)
		return
	}
	b.prompt(ctx, to, user, ms[0].MessageID, text)
}

// openTopic creates the topic of a chat and records it.
func (b *Bot) openTopic(ctx context.Context, f *forum, s hub.Session, name string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	thread, err := b.api.CreateForumTopic(cctx, f.chatID, name)
	if err != nil {
		return 0, err
	}
	if thread == 0 {
		return 0, errors.New("telegram createForumTopic: no thread ID")
	}
	b.saveTopic(cctx, f, hub.TelegramTopic{ChatID: f.chatID, ThreadID: thread, User: f.user, BackendID: s.BackendID,
		SessionID: s.ID, Name: name, Seen: b.now()})
	b.logger.Info("Created a topic", "chat", f.chatID, "thread", thread, "session", s.ID)
	return thread, nil
}

// saveTopic records a topic in the store and in f.
func (b *Bot) saveTopic(ctx context.Context, f *forum, t hub.TelegramTopic) {
	if err := b.state.SaveTelegramTopic(ctx, t); err != nil {
		b.logger.Error("Failed to store a topic", "chat", t.ChatID, "error", err)
	}
	ref := hub.SessionRef{BackendID: t.BackendID, SessionID: t.SessionID}
	b.fmu.Lock()
	defer b.fmu.Unlock()
	if old, ok := f.topics[ref]; ok && old.ThreadID != t.ThreadID {
		delete(f.threads, old.ThreadID)
	}
	f.topics[ref] = t
	if t.ThreadID != 0 {
		f.threads[t.ThreadID] = ref
	}
}

// ensureTopic attaches the chat of the topic to, and reports whether it
// is attached. A topic that someone made in Telegram gets a new chat,
// named after the topic. b.loop is held.
func (b *Bot) ensureTopic(ctx context.Context, f *forum, to dest, user string, m *Message) bool {
	st := b.chat(to)
	if st.att != nil && st.user == user {
		return true
	}
	if ref, ok := b.threadRef(f, to.thread); ok {
		err := b.attach(ctx, to, user, ref.BackendID, ref.SessionID, hub.SinceNow)
		if errors.Is(err, hub.ErrUnknownSession) {
			b.sendText(ctx, to, "This chat is gone from the hub. Send a message in "+generalName(to.chat)+" to start a new one.")
			return false
		}
		if err != nil {
			b.reportErr(ctx, to, user, "attach", err)
			return false
		}
		return true
	}
	backendID, ok := b.pickBackend(ctx, to, user, "")
	if !ok {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	s, err := b.svc.NewSession(cctx, user, backendID)
	if err != nil {
		b.reportErr(ctx, to, user, "start session", err)
		return false
	}
	name := ""
	if r := m.ReplyToMessage; r != nil && r.ForumTopicCreated != nil {
		name = r.ForumTopicCreated.Name
	}
	if name != "" {
		if err := b.svc.RenameSession(cctx, user, s.BackendID, s.ID, name); err != nil {
			b.logger.Warn("Failed to name a chat after its topic", "error", err)
		}
	}
	b.saveTopic(cctx, f, hub.TelegramTopic{ChatID: f.chatID, ThreadID: to.thread, User: f.user, BackendID: s.BackendID,
		SessionID: s.ID, Name: name, Seen: b.now()})
	if err := b.attach(ctx, to, user, s.BackendID, s.ID, 0); err != nil {
		b.reportErr(ctx, to, user, "attach", err)
		return false
	}
	return true
}

// topicGone notes a topic that Telegram refused a message for; the next
// sync drops it.
func (b *Bot) topicGone(to dest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gone[to] = true
}

// syncTopics keeps the topics of every linked group in step with the
// chats until ctx ends: it adds topics for new chats, renames them, and
// follows the chats that run.
func (b *Bot) syncTopics(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	if b.state == nil {
		return
	}
	t := time.NewTicker(b.cfg.TopicSync)
	defer t.Stop()
	for {
		me := b.refreshMe(ctx)
		dm := b.dmTopics()
		if me != nil && !dm {
			b.dropDMs(ctx)
		}
		b.fmu.Lock()
		forums := make([]*forum, 0, len(b.forums))
		for _, f := range b.forums {
			if isDM(f.chatID) && !dm {
				continue
			}
			forums = append(forums, f)
		}
		b.fmu.Unlock()
		for _, f := range forums {
			if ctx.Err() != nil {
				return
			}
			b.syncForum(ctx, f)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncForum brings one linked group's topics in step with its user's
// chats.
func (b *Bot) syncForum(ctx context.Context, f *forum) {
	cctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	sessions, err := b.svc.Sessions(cctx, f.user)
	if err != nil {
		b.logger.Warn("Failed to list chats for topics", "user", f.user, "error", err)
		return
	}
	b.noteSources(sessions)
	places := b.places(cctx, f.user, sessions)
	b.loop.Lock()
	defer b.loop.Unlock()
	if b.forum(f.chatID) != f {
		return
	}
	b.dropGone(cctx, f)
	b.deleteMarked(cctx, f)
	running := map[hub.SessionRef]bool{}
	listed := make(map[hub.SessionRef]bool, len(sessions))
	now := b.now()
	for _, s := range sessions {
		ref := hub.SessionRef{BackendID: s.BackendID, SessionID: s.ID}
		listed[ref] = true
		if !topicSource(s.Source) || hub.IsBackground(s.ID) || cctx.Err() != nil {
			continue
		}
		p, ok := places[ref]
		place := placeName(p, ok)
		if s.Source == sourceCron {
			place = "scheduled"
		}
		name := topicName(s.Title, place)
		t, ok := b.topic(f, ref)
		if ok && t.Closed && !s.Archived && t.ThreadID != 0 {
			t = b.reopenTopic(cctx, f, t)
		}
		switch {
		case s.Archived:
			if ok && t.ThreadID != 0 && !t.Closed {
				b.closeTopic(cctx, f, t)
			}
			continue
		case !ok && (s.UpdatedAt.After(f.linkedAt) || s.Running), ok && t.ThreadID == 0 && s.Running && now.Sub(t.Seen) > topicIdle:
			thread, err := b.openTopic(cctx, f, s, name)
			if err != nil {
				b.logger.Warn("Failed to create a topic", "chat", f.chatID, "error", err)
				continue
			}
			t, ok = hub.TelegramTopic{ThreadID: thread}, true
		case ok && t.ThreadID != 0 && s.Title != "" && name != t.Name:
			b.renameTopic(cctx, f, t, name)
		}
		if ok && t.ThreadID != 0 && s.Running {
			running[ref] = true
			b.follow(context.WithoutCancel(ctx), f, dest{chat: f.chatID, thread: t.ThreadID}, ref)
		}
	}
	b.detachIdle(f, running)
	b.reconcile(cctx, f, listed)
}

func (b *Bot) renameTopic(ctx context.Context, f *forum, t hub.TelegramTopic, name string) {
	err := b.api.EditForumTopic(ctx, f.chatID, t.ThreadID, name)
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.TopicGone():
		b.dropTopic(ctx, f, t.ThreadID)
	case err != nil:
		b.logger.Warn("Failed to rename a topic", "chat", f.chatID, "thread", t.ThreadID, "error", err)
	default:
		t.Name = name
		b.saveTopic(ctx, f, t)
	}
}

// follow attaches a running chat's topic that is not attached. b.loop is
// held.
func (b *Bot) follow(ctx context.Context, f *forum, to dest, ref hub.SessionRef) {
	st := b.chat(to)
	if st.att != nil && !st.att.closed() {
		return
	}
	since := hub.SinceNow
	if st.att != nil && st.att.sessionID == ref.SessionID && st.att.backendID == ref.BackendID {
		since = st.att.lastSeq.Load()
	}
	if err := b.attach(ctx, to, f.user, ref.BackendID, ref.SessionID, since); err != nil {
		b.logger.Warn("Failed to follow a running chat", "chat", f.chatID, "thread", to.thread, "error", err)
	}
}

// detachIdle detaches the topics of f whose chats stopped running a while
// ago and show no prompt. b.loop is held.
func (b *Bot) detachIdle(f *forum, running map[hub.SessionRef]bool) {
	now := b.now()
	for to, st := range b.chats {
		a := st.att
		if to.chat != f.chatID || to.thread == 0 || a == nil {
			continue
		}
		if running[hub.SessionRef{BackendID: a.backendID, SessionID: a.sessionID}] || a.prompts.Load() > 0 ||
			now.Sub(time.Unix(0, a.lastEvent.Load())) < topicIdle {
			continue
		}
		a.stop()
		delete(b.chats, to)
	}
}

// dropGone drops the topics of f that Telegram refused messages for.
// b.loop is held.
func (b *Bot) dropGone(ctx context.Context, f *forum) {
	b.mu.Lock()
	var gone []int64
	for to := range b.gone {
		if to.chat == f.chatID {
			gone = append(gone, to.thread)
			delete(b.gone, to)
		}
	}
	b.mu.Unlock()
	for _, thread := range gone {
		b.dropTopic(ctx, f, thread)
	}
}

// dropTopic forgets a topic that was deleted or closed in Telegram. Its
// chat gets a new topic when it runs again after topicIdle. b.loop is
// held.
func (b *Bot) dropTopic(ctx context.Context, f *forum, thread int64) {
	to := dest{chat: f.chatID, thread: thread}
	if st, ok := b.chats[to]; ok {
		if st.att != nil {
			st.att.stop()
		}
		delete(b.chats, to)
	}
	ref, ok := b.threadRef(f, thread)
	if !ok {
		return
	}
	t, _ := b.topic(f, ref)
	t.ThreadID, t.Seen = 0, b.now()
	b.saveTopic(ctx, f, t)
	b.logger.Info("Dropped a topic that is gone", "chat", f.chatID, "thread", thread)
}

// reconcileEvery is how often the bot checks that the chats of a group's
// topics still exist, and aliveFor how long it trusts a chat it found.
const (
	reconcileEvery = 5 * time.Minute
	aliveFor       = time.Hour
	// maxChecks bounds the chats one check asks the backends about.
	maxChecks = 20
	// generalThread is the thread of a forum's General topic, which the
	// bot never closes or deletes.
	generalThread = 1
)

// reconcile deletes the topics of f whose chats are gone, such as chats
// deleted while the bot did not follow them. It runs at the first sync
// and every reconcileEvery after. A chat in listed exists; the others are
// asked about one by one, since the list holds only recent chats and
// leaves out a backend it cannot reach. b.loop is held.
func (b *Bot) reconcile(ctx context.Context, f *forum, listed map[hub.SessionRef]bool) {
	checker, ok := b.svc.(hub.SessionChecker)
	now := b.now()
	if !ok || now.Sub(f.reconciled) < reconcileEvery {
		return
	}
	f.reconciled = now
	b.fmu.Lock()
	topics := make([]hub.TelegramTopic, 0, len(f.topics))
	for _, t := range f.topics {
		topics = append(topics, t)
	}
	b.fmu.Unlock()
	checks := 0
	for _, t := range topics {
		ref := hub.SessionRef{BackendID: t.BackendID, SessionID: t.SessionID}
		if listed[ref] || now.Sub(f.alive[ref]) < aliveFor || ctx.Err() != nil {
			continue
		}
		if checks++; checks > maxChecks {
			return
		}
		exists, err := checker.SessionExists(ctx, f.user, t.BackendID, t.SessionID)
		switch {
		case err != nil:
			b.logger.Debug("Could not check a topic's chat", "chat", f.chatID, "session", t.SessionID, "error", err)
		case exists:
			f.alive[ref] = now
		default:
			b.deleteTopic(ctx, f, t)
		}
	}
}

// markDeleted notes the topic of a chat that was deleted; the next sync
// deletes it.
func (b *Bot) markDeleted(to dest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted[to] = true
}

// deleteMarked deletes the topics of f whose chats were deleted, once the
// backend confirms it. b.loop is held.
func (b *Bot) deleteMarked(ctx context.Context, f *forum) {
	b.mu.Lock()
	var threads []int64
	for to := range b.deleted {
		if to.chat == f.chatID {
			threads = append(threads, to.thread)
			delete(b.deleted, to)
		}
	}
	b.mu.Unlock()
	checker, ok := b.svc.(hub.SessionChecker)
	for _, thread := range threads {
		ref, found := b.threadRef(f, thread)
		if !found {
			continue
		}
		if ok {
			if exists, err := checker.SessionExists(ctx, f.user, ref.BackendID, ref.SessionID); err != nil || exists {
				continue
			}
		}
		t, _ := b.topic(f, ref)
		b.deleteTopic(ctx, f, t)
	}
}

// deleteTopic deletes the topic of a chat that is gone, and forgets it.
// It never deletes General. b.loop is held.
func (b *Bot) deleteTopic(ctx context.Context, f *forum, t hub.TelegramTopic) {
	if t.ThreadID != 0 && (t.ThreadID != generalThread || isDM(f.chatID)) {
		err := b.api.DeleteForumTopic(ctx, f.chatID, t.ThreadID)
		var apiErr *APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.TopicMissing():
		case errors.As(err, &apiErr) && apiErr.NoRights():
			b.noRights(ctx, f, "delete the topics of deleted chats", err)
			return
		case err != nil:
			b.logger.Warn("Failed to delete a topic", "chat", f.chatID, "thread", t.ThreadID, "error", err)
			return
		}
	}
	b.forgetTopic(ctx, f, t)
	b.logger.Info("Deleted the topic of a deleted chat", "chat", f.chatID, "thread", t.ThreadID, "session", t.SessionID)
}

// forgetTopic drops a topic from the store and from f, and detaches its
// stream. b.loop is held.
func (b *Bot) forgetTopic(ctx context.Context, f *forum, t hub.TelegramTopic) {
	if t.ThreadID != 0 {
		to := dest{chat: f.chatID, thread: t.ThreadID}
		if st, ok := b.chats[to]; ok {
			if st.att != nil {
				st.att.stop()
			}
			delete(b.chats, to)
		}
	}
	if err := b.state.DeleteTelegramTopic(ctx, f.chatID, t.BackendID, t.SessionID); err != nil {
		b.logger.Error("Failed to forget a topic", "chat", f.chatID, "error", err)
	}
	ref := hub.SessionRef{BackendID: t.BackendID, SessionID: t.SessionID}
	b.fmu.Lock()
	defer b.fmu.Unlock()
	if cur, ok := f.topics[ref]; ok {
		delete(f.threads, cur.ThreadID)
	}
	delete(f.topics, ref)
	delete(f.alive, ref)
}

// closeTopic closes the topic of an archived chat. b.loop is held.
//
// Telegram cannot close a topic of a private chat, so there the bot marks
// the topic's name instead.
func (b *Bot) closeTopic(ctx context.Context, f *forum, t hub.TelegramTopic) {
	if t.ThreadID == generalThread && !isDM(f.chatID) {
		return
	}
	to := dest{chat: f.chatID, thread: t.ThreadID}
	if st, ok := b.chats[to]; ok {
		if st.att != nil {
			st.att.stop()
		}
		delete(b.chats, to)
	}
	var err error
	if isDM(f.chatID) {
		name := archivedName(t.Name)
		if err = b.api.EditForumTopic(ctx, f.chatID, t.ThreadID, name); err == nil {
			t.Name = name
		}
	} else {
		err = b.api.CloseForumTopic(ctx, f.chatID, t.ThreadID)
	}
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.TopicMissing():
		b.dropTopic(ctx, f, t.ThreadID)
		return
	case errors.As(err, &apiErr) && apiErr.NoRights():
		b.noRights(ctx, f, "close the topics of archived chats", err)
		return
	case err != nil:
		b.logger.Warn("Failed to close a topic", "chat", f.chatID, "thread", t.ThreadID, "error", err)
		return
	}
	t.Closed = true
	b.saveTopic(ctx, f, t)
}

// reopenTopic reopens the topic of a chat that is no longer archived and
// returns it as it is now. b.loop is held.
func (b *Bot) reopenTopic(ctx context.Context, f *forum, t hub.TelegramTopic) hub.TelegramTopic {
	var err error
	if isDM(f.chatID) {
		name := strings.TrimSuffix(t.Name, archivedSuffix)
		if err = b.api.EditForumTopic(ctx, f.chatID, t.ThreadID, name); err == nil {
			t.Name = name
		}
	} else {
		err = b.api.ReopenForumTopic(ctx, f.chatID, t.ThreadID)
	}
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.TopicMissing():
		b.dropTopic(ctx, f, t.ThreadID)
		t, _ = b.topic(f, hub.SessionRef{BackendID: t.BackendID, SessionID: t.SessionID})
		return t
	case errors.As(err, &apiErr) && apiErr.NoRights():
		b.noRights(ctx, f, "reopen the topics of chats taken out of the archive", err)
		return t
	case err != nil:
		b.logger.Warn("Failed to reopen a topic", "chat", f.chatID, "thread", t.ThreadID, "error", err)
		return t
	}
	t.Closed = false
	b.saveTopic(ctx, f, t)
	return t
}

// noRights logs, once per group, that the bot may not manage its topics,
// and says so in General. b.loop is held.
func (b *Bot) noRights(ctx context.Context, f *forum, what string, err error) {
	if f.toldRights {
		return
	}
	f.toldRights = true
	b.logger.Warn("No rights to manage topics", "chat", f.chatID, "error", err)
	b.sendText(ctx, dest{chat: f.chatID}, "I could not "+what+". Make me an admin of this group with the "+
		"\"Manage Topics\" and \"Delete Messages\" rights.")
}
