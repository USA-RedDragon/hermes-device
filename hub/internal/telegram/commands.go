package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const (
	// cbModel is a model button of /model: "md:<gen>:<i>". cbModelYes
	// confirms a model that Hermes asked about: "my:<gen>".
	cbModel    = "md"
	cbModelYes = "my"
	// cbSubagents is a Subagents button of /model: "sa:<gen>:<i>".
	cbSubagents = "sa"
	// maxModelButtons bounds the model buttons /model shows.
	maxModelButtons = 24
	// bgCompleteEvent ends a /bg prompt with its answer.
	bgCompleteEvent = "background.complete"
)

// modelPick is a model that /model offers.
type modelPick struct {
	provider, model string
}

// modelMenu is what /model showed a chat, for its buttons.
type modelMenu struct {
	gen   int
	picks []modelPick
	// confirm is a pick that Hermes asked to confirm, and token the
	// confirmation it gave.
	confirm modelPick
	token   string
	// subagents are the Subagents choices it showed.
	subagents []string
}

// model handles /model: with no argument it shows the chat's model and a
// button for each model it may switch to; "/model NAME" or "/model
// provider/NAME" switches.
func (b *Bot) model(ctx context.Context, to dest, user, arg string) {
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ms, err := b.svc.Models(cctx, user, a.backendID, a.sessionID)
	if err != nil {
		b.reportErr(ctx, to, user, "list models", err)
		return
	}
	st := b.chat(to)
	st.models.gen++
	st.models.picks, st.models.token = nil, ""
	if arg != "" {
		pick, found := findModel(ms, arg)
		if !found {
			b.sendText(ctx, to, "No model "+arg+" here. Send /model to list them.")
			return
		}
		b.setModel(ctx, to, user, a, pick, "")
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Model:</b> <code>%s</code>", escapeHTML(cmpOr(ms.Model, "unknown")))
	if ms.Provider != "" {
		fmt.Fprintf(&sb, " <i>(%s)</i>", escapeHTML(ms.Provider))
	}
	var rows [][]InlineButton
	total := 0
	groups := map[string]bool{}
	for _, p := range ms.Providers {
		groups[cmpOr(p.Group, p.Name)] = true
	}
	for _, p := range ms.Providers {
		for _, m := range p.Models {
			total++
			if len(st.models.picks) >= maxModelButtons {
				continue
			}
			st.models.picks = append(st.models.picks, modelPick{provider: p.ID, model: m.ID})
			label := cmpOr(m.Label, m.ID)
			if len(groups) > 1 {
				label = cmpOr(p.Group, p.Name) + ": " + label
			}
			if p.ID == ms.Provider && m.ID == ms.Model {
				label = "• " + label
			}
			rows = append(rows, []InlineButton{{Text: truncate(label, 60),
				CallbackData: fmt.Sprintf("%s:%d:%d", cbModel, st.models.gen, len(st.models.picks)-1)}})
		}
	}
	switch {
	case total == 0:
		sb.WriteString("\nThis chat has no other model to switch to.")
	case total > len(st.models.picks):
		fmt.Fprintf(&sb, "\nPick one, or send /model NAME. %d more are not shown.", total-len(st.models.picks))
	default:
		sb.WriteString("\nPick one, or send /model NAME.")
	}
	rows = append(rows, b.subagentsMenu(ctx, &sb, st, user, a)...)
	var kb *InlineKeyboard
	if len(rows) > 0 {
		kb = &InlineKeyboard{InlineKeyboard: rows}
	}
	b.sendHTML(ctx, to, sb.String(), kb)
}

// subagentsMenu adds the chat's Subagents choice to /model, with a button
// for each choice, when its backend has the subagent-routes plugin.
func (b *Bot) subagentsMenu(ctx context.Context, sb *strings.Builder, st *chatState, user string, a *attachment) [][]InlineButton {
	st.models.subagents = nil
	if b.subagents == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	sa, err := b.subagents.ChatSubagents(cctx, user, a.backendID, a.sessionID)
	if err != nil {
		if !errors.Is(err, hub.ErrNoSubagentRoutes) {
			b.logger.Warn("Failed to read a chat's Subagents choice", "error", err.Error())
		}
		return nil
	}
	cur, _ := sa.Current()
	fmt.Fprintf(sb, "\n\n<b>Subagents:</b> %s", escapeHTML(cmpOr(cur.Label, sa.Choice)))
	if cur.Line != "" {
		fmt.Fprintf(sb, "\n<i>%s</i>", escapeHTML(cur.Line))
	}
	var rows [][]InlineButton
	for _, c := range sa.Choices {
		st.models.subagents = append(st.models.subagents, c.ID)
		label := "Subagents: " + c.Label
		if c.ID == sa.Choice {
			label = "• " + label
		}
		rows = append(rows, []InlineButton{{Text: truncate(label, 60),
			CallbackData: fmt.Sprintf("%s:%d:%d", cbSubagents, st.models.gen, len(st.models.subagents)-1)}})
	}
	return rows
}

// pressSubagents handles a Subagents button of /model.
func (b *Bot) pressSubagents(ctx context.Context, q *CallbackQuery, to dest, user string, parts []string) {
	st := b.chat(to)
	gen, err1 := strconv.Atoi(parts[1])
	i, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || gen != st.models.gen || i < 0 || i >= len(st.models.subagents) || b.subagents == nil {
		b.answer(ctx, q.ID, "That list is out of date. Send /model again.")
		return
	}
	a := st.att
	if a == nil || st.user != user {
		b.answer(ctx, q.ID, msgNoAttach)
		return
	}
	b.answer(ctx, q.ID, "")
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	sa, err := b.subagents.SetChatSubagents(cctx, user, a.backendID, a.sessionID, st.models.subagents[i])
	if err != nil {
		b.reportErr(ctx, to, user, "set subagents", err)
		return
	}
	cur, _ := sa.Current()
	text := "Subagents set to <b>" + escapeHTML(cmpOr(cur.Label, sa.Choice)) + "</b>."
	if cur.Line != "" {
		text += " " + escapeHTML(cur.Line)
	}
	b.sendHTML(ctx, to, text, nil)
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// findModel finds a model by "NAME", "provider/NAME" or "provider:NAME",
// preferring the chat's own provider for a bare name.
func findModel(ms hub.Models, arg string) (modelPick, bool) {
	arg = strings.TrimSpace(arg)
	want := ""
	for _, sep := range []string{":", "/"} {
		if prov, name, ok := strings.Cut(arg, sep); ok {
			for _, p := range ms.Providers {
				if strings.EqualFold(p.ID, prov) {
					want, arg = p.ID, name
				}
			}
		}
		if want != "" {
			break
		}
	}
	var found []modelPick
	for _, p := range ms.Providers {
		if want != "" && p.ID != want {
			continue
		}
		for _, m := range p.Models {
			if strings.EqualFold(m.ID, arg) {
				found = append(found, modelPick{provider: p.ID, model: m.ID})
			}
		}
	}
	for _, f := range found {
		if f.provider == ms.Provider {
			return f, true
		}
	}
	if len(found) > 0 {
		return found[0], true
	}
	return modelPick{}, false
}

// setModel switches the chat's model and says how it went. token is the
// confirmation of a model that Hermes asked about.
func (b *Bot) setModel(ctx context.Context, to dest, user string, a *attachment, pick modelPick, token string) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := b.svc.SetModel(cctx, user, a.backendID, a.sessionID, pick.provider, pick.model, token)
	if err != nil {
		b.reportErr(ctx, to, user, "set model", err)
		return
	}
	name := "<code>" + escapeHTML(pick.model) + "</code>"
	switch res.Status {
	case hub.ModelConfirm:
		st := b.chat(to)
		st.models.confirm, st.models.token = pick, res.Confirm
		msg := cmpOr(res.Message, "This model needs a confirmation.")
		b.sendHTML(ctx, to, escapeHTML(msg), &InlineKeyboard{InlineKeyboard: [][]InlineButton{{
			{Text: "Use it anyway", CallbackData: fmt.Sprintf("%s:%d", cbModelYes, st.models.gen)}}}})
	case hub.ModelDeferred:
		b.sendHTML(ctx, to, "The model changes to "+name+" when the current reply is done.", nil)
	case hub.ModelUnchanged:
		b.sendHTML(ctx, to, name+" is already in use.", nil)
	default:
		b.sendHTML(ctx, to, "Model changed to "+name+".", nil)
	}
}

// modelButton reports whether a button's data is one of /model: a model,
// its confirmation or a Subagents choice.
func modelButton(parts []string) bool {
	switch len(parts) {
	case 2:
		return parts[0] == cbModelYes
	case 3:
		return parts[0] == cbModel || parts[0] == cbSubagents
	}
	return false
}

// pressModel handles a button of /model: a model, its confirmation or a
// Subagents choice.
func (b *Bot) pressModel(ctx context.Context, q *CallbackQuery, to dest, user string, parts []string) {
	if parts[0] == cbSubagents {
		b.pressSubagents(ctx, q, to, user, parts)
		return
	}
	st := b.chat(to)
	gen, err := strconv.Atoi(parts[1])
	if err != nil || gen != st.models.gen {
		b.answer(ctx, q.ID, "That list is out of date. Send /model again.")
		return
	}
	a := st.att
	if a == nil || st.user != user {
		b.answer(ctx, q.ID, msgNoAttach)
		return
	}
	if parts[0] == cbModelYes {
		if st.models.token == "" {
			b.answer(ctx, q.ID, "That list is out of date. Send /model again.")
			return
		}
		b.answer(ctx, q.ID, "")
		pick, token := st.models.confirm, st.models.token
		st.models.token = ""
		b.setModel(ctx, to, user, a, pick, token)
		return
	}
	i, err := strconv.Atoi(parts[2])
	if err != nil || i < 0 || i >= len(st.models.picks) {
		b.answer(ctx, q.ID, "That list is out of date. Send /model again.")
		return
	}
	b.answer(ctx, q.ID, "")
	b.setModel(ctx, to, user, a, st.models.picks[i], "")
}

// background handles "/bg TEXT": a separate agent runs TEXT with the
// chat's settings, and its answer comes when it is done.
func (b *Bot) background(ctx context.Context, to dest, user, arg string) {
	act, ok := b.svc.(hub.Activity)
	if !ok {
		b.sendText(ctx, to, "This hub cannot run background prompts.")
		return
	}
	if strings.TrimSpace(arg) == "" {
		b.sendText(ctx, to, "Send /bg and a prompt, such as /bg check the build. Talaria answers it in the background, and the answer comes here.")
		return
	}
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := act.Background(cctx, user, a.backendID, a.sessionID, arg); err != nil {
		b.reportErr(ctx, to, user, "background", err)
		return
	}
	b.sendText(ctx, to, "Started in the background. The answer comes here when it is done.")
}

// compressTimeout bounds /compress, which waits for Hermes to summarize.
const compressTimeout = 10 * time.Minute

// compress handles /compress and /compact: Hermes summarizes the chat's
// older messages, and its report comes back as the answer.
func (b *Bot) compress(ctx context.Context, to dest, user, arg string) {
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, compressTimeout)
	defer cancel()
	res, err := b.svc.Prompt(cctx, user, a.backendID, a.sessionID, strings.TrimSpace("/"+cmdCompress+" "+arg))
	if err != nil {
		b.reportErr(ctx, to, user, "compress", err)
		return
	}
	b.sendText(ctx, to, cmpOr(res.Output, "Compressed."))
}

// backgroundResult shows the answer of a /bg prompt.
func (r *renderer) backgroundResult(ctx context.Context, ev hub.Event) {
	var p struct {
		TaskID string `json:"task_id"`
		Text   string `json:"text"`
	}
	if !r.decode(ev, &p) || p.TaskID == "" {
		return
	}
	text := strings.TrimSpace(p.Text)
	head := "<b>Background answer</b>\n"
	if strings.HasPrefix(text, "error: ") {
		head = "<b>Background prompt failed</b>\n"
	}
	if text == "" {
		r.send(ctx, head+"<i>No answer.</i>", nil)
		return
	}
	for i, chunk := range RenderMarkdown(text, MaxMessageLen-len(head)) {
		if i == 0 {
			chunk = head + chunk
		}
		if r.send(ctx, chunk, nil) == 0 {
			break
		}
	}
}

// Commands the bot takes, besides cmdLink and cmdSkip.
const (
	cmdNew      = "new"
	cmdStop     = "stop"
	cmdStatus   = "status"
	cmdModel    = "model"
	cmdBg       = "bg"
	cmdCompress = "compress"
	cmdSessions = "sessions"
	cmdBackends = "backends"
	cmdDevices  = "devices"
	cmdWhere    = "where"
	cmdMove     = "move"
	cmdUnlink   = "unlink"
	cmdHelp     = "help"
	helpLine    = "List the commands"
)

// commonCommands are the menu entries of private chats and linked groups.
func (b *Bot) commonCommands() []BotCommand {
	out := []BotCommand{
		{cmdNew, "Start a new chat"},
		{cmdStop, "Interrupt the running turn"},
		{cmdModel, "Show or switch the chat's model"},
		{cmdBg, "Run a prompt in the background"},
		{cmdCompress, "Summarize older messages to free context"},
		{cmdStatus, "Show the model and how full its context is"},
	}
	if b.computers != nil {
		out = append(out, BotCommand{cmdDevices, "List your computers"}, BotCommand{cmdWhere, "Show where this chat runs"},
			BotCommand{cmdMove, "Run this chat on a computer"})
	}
	if b.prompts != nil {
		out = append(out, BotCommand{cmdPrompts, "Get the text of a saved prompt"})
	}
	return out
}

// privateCommands is the menu of private chats.
func (b *Bot) privateCommands() []BotCommand {
	out := append(b.commonCommands(), BotCommand{cmdSessions, "List your chats to attach one"},
		BotCommand{cmdBackends, "List your backends"})
	if b.state != nil {
		out = append(out, BotCommand{cmdLink, "Link a group with Topics"}, BotCommand{cmdUnlink, "Stop using your linked group"})
	}
	return append(out, BotCommand{cmdHelp, helpLine})
}

// forumCommands is the menu of a linked group.
func (b *Bot) forumCommands() []BotCommand {
	return append(b.commonCommands(), BotCommand{cmdBackends, "List your backends"},
		BotCommand{cmdUnlink, "Stop using this group"}, BotCommand{cmdHelp, helpLine})
}

// groupCommands is the menu of the other groups: linking one.
func (b *Bot) groupCommands() []BotCommand {
	if b.state == nil {
		return []BotCommand{{cmdHelp, helpLine}}
	}
	return []BotCommand{{cmdLink, "Give each of your chats a topic here"}, {cmdHelp, helpLine}}
}

// setCommands sets the command menus: one for private chats, one for
// groups, and one for each linked group. A failure only costs the menu.
func (b *Bot) setCommands(ctx context.Context) {
	set := func(scope CommandScope, cmds []BotCommand) {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		if err := b.api.SetCommands(cctx, scope, cmds); err != nil {
			b.logger.Warn("Failed to set the command menu", "scope", scope.Type, "chat", scope.ChatID, "error", err)
		}
	}
	set(CommandScope{Type: "all_private_chats"}, b.privateCommands())
	set(CommandScope{Type: "all_group_chats"}, b.groupCommands())
	for _, id := range b.forumIDs() {
		if id < 0 {
			set(CommandScope{Type: "chat", ChatID: id}, b.forumCommands())
		}
	}
	b.logger.Info("Set the Telegram command menus")
}

// forumIDs are the chat IDs of the linked groups and private chats with
// topics.
func (b *Bot) forumIDs() []int64 {
	b.fmu.Lock()
	defer b.fmu.Unlock()
	out := make([]int64, 0, len(b.forums))
	for id := range b.forums {
		out = append(out, id)
	}
	return out
}

// groupMenu sets or removes the menu of a group that was linked or
// unlinked.
func (b *Bot) groupMenu(ctx context.Context, chatID int64, linked bool) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	scope := CommandScope{Type: "chat", ChatID: chatID}
	var err error
	if linked {
		err = b.api.SetCommands(cctx, scope, b.forumCommands())
	} else {
		err = b.api.DeleteCommands(cctx, scope)
	}
	if err != nil {
		b.logger.Warn("Failed to set a group's command menu", "chat", chatID, "error", err)
	}
}
