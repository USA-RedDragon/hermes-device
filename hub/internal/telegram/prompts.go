package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/notify"
)

// cbNotice prefixes the buttons of a notice sent to a person's private
// chat; its key is the stored notice's ID.
const cbNotice = "nq"

// Button tokens after "<prefix>:<key>:": a choice index, "t<i>" to toggle
// a choice of a multi-select question, "d" to send the toggled choices,
// "o" to type an answer and "s" to skip.
const (
	tokSend  = "d"
	tokOther = "o"
	tokSkip  = "s"
)

// promptTarget is a prompt that Telegram buttons answer.
type promptTarget struct {
	prefix    string
	key       string
	chatID    int64
	backendID string
	sessionID string
	prompt    hub.PromptInfo
	// text is the message's HTML, kept to show the outcome under it.
	text string
	// owner is the only person who may answer, for a notice; "" lets
	// anyone who may use the backend answer, as in an attached chat.
	owner string
}

func (t promptTarget) id() string { return t.prefix + ":" + t.key }

// approvalLabel names an approval choice on its button.
func approvalLabel(choice string) string {
	switch choice {
	case hub.ChoiceOnce:
		return "Allow"
	case hub.ChoiceSession:
		return "Allow in this chat"
	case hub.ChoiceAlways:
		return "Always allow"
	case hub.ChoiceDeny:
		return "Deny"
	}
	return choice
}

// approvalVerdict says what an approval choice did.
func approvalVerdict(choice string) string {
	switch choice {
	case hub.ChoiceOnce:
		return "Allowed"
	case hub.ChoiceSession:
		return "Allowed in this chat"
	case hub.ChoiceAlways:
		return "Always allowed"
	case hub.ChoiceDeny:
		return "Denied"
	}
	return "Answered"
}

// promptHTML describes a prompt under head.
func promptHTML(head string, p *hub.PromptInfo) string {
	var b strings.Builder
	b.WriteString(head)
	if p == nil {
		return b.String()
	}
	if p.Command != "" {
		b.WriteString("\n<pre>" + escapeHTML(truncate(p.Command, 2500)) + "</pre>")
	}
	if p.Description != "" {
		b.WriteString("\n" + escapeHTML(truncate(p.Description, 1000)))
	}
	if p.Question != "" {
		b.WriteString("\n" + escapeHTML(truncate(p.Question, 2500)))
	}
	if p.Kind == hub.PromptClarify {
		for i, c := range p.Choices {
			fmt.Fprintf(&b, "\n%d. %s", i+1, escapeHTML(truncate(c, 300)))
		}
		if p.Multi {
			b.WriteString("\n<i>Pick one or more, then press Send.</i>")
		}
	}
	return b.String()
}

// maxButtons is how many choices get a button of their own.
const maxButtons = 10

// promptKeyboard is the buttons that answer p. picks marks the toggled
// choices of a multi-select question.
func promptKeyboard(prefix, key string, p hub.PromptInfo, picks []bool) *InlineKeyboard {
	cb := func(tok string) string { return prefix + ":" + key + ":" + tok }
	var rows [][]InlineButton
	if p.Kind != hub.PromptClarify {
		choices := approvalChoices(p)
		row := make([]InlineButton, 0, len(choices))
		for i, c := range choices {
			row = append(row, InlineButton{Text: approvalLabel(c), CallbackData: cb(strconv.Itoa(i))})
		}
		if len(row) > 2 {
			rows = append(rows, row[:2], row[2:])
		} else {
			rows = append(rows, row)
		}
		return &InlineKeyboard{InlineKeyboard: rows}
	}
	for i, c := range p.Choices[:min(len(p.Choices), maxButtons)] {
		label := truncate(fmt.Sprintf("%d. %s", i+1, c), 40)
		tok := strconv.Itoa(i)
		if p.Multi {
			tok = "t" + tok
			if i < len(picks) && picks[i] {
				label = "✓ " + label
			}
		}
		rows = append(rows, []InlineButton{{Text: label, CallbackData: cb(tok)}})
	}
	last := []InlineButton{}
	if p.Multi {
		last = append(last, InlineButton{Text: "Send", CallbackData: cb(tokSend)})
	}
	other := "Type an answer"
	if len(p.Choices) == 0 {
		other = "Answer"
	}
	last = append(last, InlineButton{Text: other, CallbackData: cb(tokOther)}, InlineButton{Text: "Skip", CallbackData: cb(tokSkip)})
	return &InlineKeyboard{InlineKeyboard: append(rows, last)}
}

// approvalChoices are the choices of an approval, or allow and deny when
// the event named none.
func approvalChoices(p hub.PromptInfo) []string {
	if len(p.Choices) == 0 {
		return []string{hub.ChoiceOnce, hub.ChoiceDeny}
	}
	return p.Choices
}

// noticeHead is the first line of a notice.
func noticeHead(m notify.Message) string {
	head := notify.Titles(m.Record.Kind)
	if m.Reminder {
		head = "Reminder: " + head
	}
	var b strings.Builder
	b.WriteString("<b>" + escapeHTML(head) + "</b>")
	if m.Title != "" {
		b.WriteString(" in <b>" + escapeHTML(truncate(m.Title, 80)) + "</b>")
	}
	if m.Backend != "" {
		b.WriteString(" on <b>" + escapeHTML(m.Backend) + "</b>")
	}
	if m.Detail != "" {
		b.WriteString("\n" + escapeHTML(m.Detail))
	}
	return b.String()
}

// SendNotice sends a notice to a person's private chat, with buttons when
// it is a prompt and a link that opens the chat in the hub.
func (b *Bot) SendNotice(ctx context.Context, chatID int64, m notify.Message) (int64, error) {
	text := promptHTML(noticeHead(m), m.Record.Prompt)
	if m.URL != "" {
		text += "\n\n<a href=\"" + escapeHTML(m.URL) + "\">Open in the hub</a>"
	}
	var kb *InlineKeyboard
	if m.Record.Prompt != nil {
		kb = promptKeyboard(cbNotice, m.Record.ID, *m.Record.Prompt, nil)
	}
	id := sendHTML(ctx, b.api, b.logger, chatID, text, kb)
	if id == 0 {
		return 0, errors.New("telegram: send failed")
	}
	return id, nil
}

// Settled takes the buttons off the messages of a prompt that got its
// answer somewhere else.
func (b *Bot) Settled(ctx context.Context, msgs []hub.NoticeMessage) {
	for _, m := range msgs {
		if err := b.api.EditMarkup(ctx, m.ChatID, m.MessageID, nil); err != nil {
			b.logger.Debug("Failed to take buttons off a notice", "chat", m.ChatID, "error", err)
		}
	}
}

// noticeTarget loads the notice a button of a private chat answers. Only
// the person it was sent to may press it, on a message it was sent in.
func (b *Bot) noticeTarget(ctx context.Context, q *CallbackQuery, user, key string) (promptTarget, string) {
	if b.notices == nil {
		return promptTarget{}, msgNotPending
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	rec, ok, err := b.notices.Notice(cctx, key)
	if err != nil {
		return promptTarget{}, errGeneric
	}
	if !ok || rec.Prompt == nil || !rec.SettledAt.IsZero() {
		return promptTarget{}, msgNotPending
	}
	if rec.User != user {
		b.logger.Warn("Notice button pressed by another user", "user", user)
		return promptTarget{}, msgNotYours
	}
	msgs, err := b.notices.NoticeMessages(cctx, key)
	if err != nil {
		return promptTarget{}, errGeneric
	}
	found := false
	for _, m := range msgs {
		found = found || (m.ChatID == q.Message.Chat.ID && m.MessageID == q.Message.MessageID)
	}
	if !found {
		return promptTarget{}, msgNotPending
	}
	return promptTarget{prefix: cbNotice, key: key, chatID: q.Message.Chat.ID, backendID: rec.BackendID, sessionID: rec.SessionID,
		prompt: *rec.Prompt, text: escapeHTML(q.Message.Text), owner: rec.User}, ""
}

// attachedTarget loads the prompt a button of an attached chat answers.
func (b *Bot) attachedTarget(q *CallbackQuery, key string) (promptTarget, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.approvals[key]
	// messageID is 0 only between the send and its bind; the random key
	// alone ties the press to this prompt then.
	if !ok || p.chatID != q.Message.Chat.ID || (p.messageID != 0 && p.messageID != q.Message.MessageID) {
		return promptTarget{}, msgNotPending
	}
	return promptTarget{prefix: cbApproval, key: key, chatID: p.chatID, backendID: p.backendID, sessionID: p.sessionID,
		prompt: p.prompt, text: p.text}, ""
}

const (
	msgNotPending = "This request is no longer pending."
	msgNotYours   = "You are not allowed to answer this request."
)

// pressPrompt handles a button of a prompt.
func (b *Bot) pressPrompt(ctx context.Context, q *CallbackQuery, user string, t promptTarget, tok string) {
	p := t.prompt
	var (
		a       hub.Answer
		verdict string
	)
	switch {
	case p.Kind == hub.PromptClarify && strings.HasPrefix(tok, "t") && p.Multi:
		b.togglePick(ctx, q, user, t, tok[1:])
		return
	case p.Kind == hub.PromptClarify && tok == tokOther:
		b.awaitAnswer(ctx, q, user, t)
		return
	case p.Kind == hub.PromptClarify && tok == tokSkip:
		a, verdict = hub.Answer{Skip: true}, "Skipped"
	case p.Kind == hub.PromptClarify && tok == tokSend && p.Multi:
		picks := b.picked(t)
		if len(picks) == 0 {
			b.answer(ctx, q.ID, "Pick at least one answer first.")
			return
		}
		a, verdict = hub.Answer{Picks: picks}, "Answered: "+strings.Join(picks, ", ")
	default:
		i, err := strconv.Atoi(tok)
		if p.Kind == hub.PromptClarify {
			if err != nil || i < 0 || i >= len(p.Choices) || p.Multi {
				b.answer(ctx, q.ID, "")
				return
			}
			a, verdict = hub.Answer{Picks: []string{p.Choices[i]}}, "Answered: "+p.Choices[i]
			break
		}
		choices := approvalChoices(p)
		if err != nil || i < 0 || i >= len(choices) {
			b.answer(ctx, q.ID, "")
			return
		}
		a, verdict = hub.Answer{Choice: choices[i]}, approvalVerdict(choices[i])
	}
	if !b.submitAnswer(ctx, q.ID, user, t, a) {
		return
	}
	b.answer(ctx, q.ID, truncate(verdict, 180))
	b.editHTML(ctx, t.chatID, q.Message.MessageID, t.text+"\n\n<i>"+escapeHTML(truncate(verdict, 300))+" by "+escapeHTML(user)+".</i>")
}

// sendAnswer sends a to Hermes as user.
func (b *Bot) sendAnswer(ctx context.Context, user string, t promptTarget, a hub.Answer) error {
	if t.owner != "" && t.owner != user {
		return hub.ErrForbidden
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if pr, ok := b.svc.(hub.Prompter); ok {
		return pr.AnswerPrompt(cctx, user, t.backendID, t.sessionID, t.prompt.ID, a)
	}
	return b.svc.RespondApproval(cctx, user, t.backendID, t.sessionID, t.prompt.ID, a.Choice == hub.ChoiceOnce)
}

// submitAnswer sends a to Hermes and reports a failure on the button. It
// returns true when Hermes took the answer.
func (b *Bot) submitAnswer(ctx context.Context, callbackID, user string, t promptTarget, a hub.Answer) bool {
	err := b.sendAnswer(ctx, user, t, a)
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Prompt answer forbidden", "user", user, "backend", t.backendID)
		b.reply(ctx, callbackID, t.chatID, msgNotYours)
		return false
	case errors.Is(err, hub.ErrUnknownApproval), errors.Is(err, hub.ErrUnknownSession):
		b.dropPrompt(t)
		b.reply(ctx, callbackID, t.chatID, msgNotPending)
		return false
	case errors.Is(err, hub.ErrBadValue):
		b.reply(ctx, callbackID, t.chatID, "Hermes did not take that answer.")
		return false
	case err != nil:
		b.logger.Error("Failed to answer a prompt", "user", user, "error", err)
		b.reply(ctx, callbackID, t.chatID, errGeneric)
		return false
	}
	b.dropPrompt(t)
	return true
}

// reportAnswerErr tells a chat why its typed answer failed.
func (b *Bot) reportAnswerErr(ctx context.Context, chatID int64, user string, t promptTarget, err error) {
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Prompt answer forbidden", "user", user, "backend", t.backendID)
		b.sendText(ctx, chatID, msgNotYours)
	case errors.Is(err, hub.ErrBadValue):
		b.sendText(ctx, chatID, "Hermes did not take that answer.")
	default:
		b.logger.Error("Failed to answer a prompt", "user", user, "error", err)
		b.sendText(ctx, chatID, errGeneric)
	}
}

// awaitTTL is how long the chat's next message counts as the answer
// after the person pressed Type an answer.
const awaitTTL = 10 * time.Minute

// reply answers a button press, or, for a typed answer, sends text.
func (b *Bot) reply(ctx context.Context, callbackID string, chatID int64, text string) {
	if callbackID != "" {
		b.answer(ctx, callbackID, text)
		return
	}
	b.sendText(ctx, chatID, text)
}

func (b *Bot) dropPrompt(t promptTarget) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.picks, t.id())
	if t.prefix == cbApproval {
		delete(b.approvals, t.key)
	}
}

func (b *Bot) togglePick(ctx context.Context, q *CallbackQuery, user string, t promptTarget, tok string) {
	i, err := strconv.Atoi(tok)
	if err != nil || i < 0 || i >= len(t.prompt.Choices) || i >= maxButtons {
		b.answer(ctx, q.ID, "")
		return
	}
	if !b.mayAnswer(ctx, user, t) {
		b.answer(ctx, q.ID, msgNotYours)
		return
	}
	b.mu.Lock()
	picks := b.picks[t.id()]
	if len(picks) != len(t.prompt.Choices) {
		picks = make([]bool, len(t.prompt.Choices))
	}
	picks[i] = !picks[i]
	b.picks[t.id()] = picks
	shown := append([]bool(nil), picks...)
	b.mu.Unlock()
	b.answer(ctx, q.ID, "")
	if err := b.api.EditMarkup(ctx, t.chatID, q.Message.MessageID, promptKeyboard(t.prefix, t.key, t.prompt, shown)); err != nil {
		b.logger.Warn("Failed to update the answer buttons", "error", err)
	}
}

// mayAnswer reports whether user may answer t: its owner, or for an
// attached chat anyone who may use its backend.
func (b *Bot) mayAnswer(ctx context.Context, user string, t promptTarget) bool {
	if t.owner != "" {
		return t.owner == user
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	list, err := b.svc.Backends(cctx, user)
	if err != nil {
		return false
	}
	for _, be := range list {
		if be.ID == t.backendID {
			return true
		}
	}
	return false
}

func (b *Bot) picked(t promptTarget) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for i, on := range b.picks[t.id()] {
		if on && i < len(t.prompt.Choices) {
			out = append(out, t.prompt.Choices[i])
		}
	}
	return out
}

// awaitAnswer makes the person's next message in the chat the answer.
func (b *Bot) awaitAnswer(ctx context.Context, q *CallbackQuery, user string, t promptTarget) {
	if !b.mayAnswer(ctx, user, t) {
		b.answer(ctx, q.ID, msgNotYours)
		return
	}
	st := b.chat(t.chatID)
	st.awaiting, st.awaitingUser, st.awaitingMsg, st.awaitingAt = &t, user, q.Message.MessageID, time.Now()
	b.answer(ctx, q.ID, "")
	b.sendText(ctx, t.chatID, "Send your answer as your next message. Send /cancel to go back.")
}

// typedAnswer answers the prompt the chat waits on with text, and reports
// whether the message was such an answer.
func (b *Bot) typedAnswer(ctx context.Context, chatID int64, user, text string) bool {
	st := b.chat(chatID)
	t := st.awaiting
	if t == nil || st.awaitingUser != user {
		return false
	}
	st.awaiting = nil
	if time.Since(st.awaitingAt) > awaitTTL {
		return false
	}
	if strings.HasPrefix(text, "/") {
		if cmd, _ := parseCommand(text); cmd == "cancel" {
			b.sendText(ctx, chatID, "Canceled. The question still waits for an answer.")
			return true
		}
		return false
	}
	err := b.sendAnswer(ctx, user, *t, hub.Answer{Text: text})
	if errors.Is(err, hub.ErrUnknownApproval) || errors.Is(err, hub.ErrUnknownSession) {
		b.dropPrompt(*t)
		b.sendText(ctx, chatID, "That question no longer waits for an answer, so your message goes to the chat.")
		return false
	}
	if err != nil {
		b.reportAnswerErr(ctx, chatID, user, *t, err)
		return true
	}
	b.dropPrompt(*t)
	b.editHTML(ctx, chatID, st.awaitingMsg, t.text+"\n\n<i>Answered by "+escapeHTML(user)+".</i>")
	b.sendText(ctx, chatID, "Sent your answer.")
	return true
}
