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
	to        dest
	backendID string
	sessionID string
	prompt    hub.PromptInfo
	// body is the message's HTML under its head, kept to show the outcome
	// with it.
	body string
	// owner is the only person who may answer, for a notice; "" lets
	// anyone who may use the backend answer, as in an attached chat.
	owner string
}

func (t promptTarget) id() string { return t.prefix + ":" + t.key }

// replyTarget is a prompt that a reply to a message answers: the
// question itself, or the message that asked for the answer. question is
// the message that shows the prompt.
type replyTarget struct {
	prefix   string
	key      string
	question int64
	at       time.Time
}

// shownNotice is a notice the bot sent, kept to show its outcome.
type shownNotice struct {
	body string
	at   time.Time
}

// keepShown is how long the bot keeps what it needs to answer replies and
// show outcomes of messages it sent.
const keepShown = 24 * time.Hour

// approvalLabel names an approval choice on its button.
func approvalLabel(choice string) string {
	switch choice {
	case hub.ChoiceOnce:
		return "Once"
	case hub.ChoiceSession:
		return "This chat"
	case hub.ChoiceAlways:
		return "Always"
	case hub.ChoiceDeny:
		return "Deny"
	}
	return choice
}

// approvalVerdict says what an approval choice did.
func approvalVerdict(choice string) string {
	switch choice {
	case hub.ChoiceOnce:
		return "Approved once"
	case hub.ChoiceSession:
		return "Approved for this chat"
	case hub.ChoiceAlways:
		return "Always approved"
	case hub.ChoiceDeny:
		return "Denied"
	}
	return "Answered"
}

// outcomeHead says what became of an approval and where it got its
// answer, such as "Allowed always (in the hub)". An outcome the hub worked
// out from the tool call has no choice, so it names no scope.
func outcomeHead(o hub.ApprovalOutcome) string {
	head := "Allowed"
	switch {
	case !o.Allowed:
		head = "Denied"
	case o.Choice == hub.ChoiceOnce:
		head = "Allowed once"
	case o.Choice == hub.ChoiceSession:
		head = "Allowed for this chat"
	case o.Choice == hub.ChoiceAlways:
		head = "Allowed always"
	}
	switch o.Via {
	case hub.AnsweredInHub:
		return head + " (in the hub)"
	case hub.AnsweredInTelegram:
		return head + " (in Telegram)"
	case hub.AnsweredOnDevice:
		if o.Device == "" {
			return head + " (on a paired device)"
		}
		return head + " (on " + o.Device + ")"
	}
	return head + " (in another app)"
}

// answerFinder is the optional part of a NoticeStore that reads the
// recorded answer to an approval.
type answerFinder interface {
	PromptAnswer(ctx context.Context, backendID, promptID string) (hub.PromptAnswer, bool, error)
}

// recordedAnswer is the outcome of the answer the hub recorded for an
// approval.
func (b *Bot) recordedAnswer(ctx context.Context, backendID, promptID string) (hub.ApprovalOutcome, bool) {
	af, ok := b.notices.(answerFinder)
	if !ok {
		return hub.ApprovalOutcome{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	a, ok, err := af.PromptAnswer(cctx, backendID, promptID)
	if err != nil {
		b.logger.Warn("Failed to read an approval's answer", "error", err)
	}
	return a.Outcome(promptID), ok
}

// promptBody describes a prompt: the command, the description, the
// question, and choices that get no button of their own.
func promptBody(p *hub.PromptInfo) string {
	if p == nil {
		return ""
	}
	var parts []string
	if p.Command != "" {
		parts = append(parts, "<pre>"+escapeHTML(truncate(p.Command, 2500))+"</pre>")
	}
	if p.Description != "" {
		parts = append(parts, escapeHTML(truncate(p.Description, 1000)))
	}
	if p.Question != "" {
		parts = append(parts, escapeHTML(truncate(p.Question, 2500)))
	}
	if p.Kind == hub.PromptClarify && len(p.Choices) > maxButtons {
		for i, c := range p.Choices {
			parts = append(parts, fmt.Sprintf("%d. %s", i+1, escapeHTML(truncate(c, 300))))
		}
	}
	return strings.Join(parts, "\n")
}

// promptFooter says how to answer a question.
func promptFooter(p hub.PromptInfo) string {
	switch {
	case p.Kind != hub.PromptClarify:
		return ""
	case len(p.Choices) == 0:
		return "\n\n<i>Reply to this message with your answer, or with /skip.</i>"
	case p.Multi:
		return "\n\n<i>Pick one or more, then press Send. Or reply to this message with your own answer.</i>"
	}
	return "\n\n<i>Or reply to this message with your own answer.</i>"
}

// outcomeHTML is a prompt's message once it got its answer: what became
// of it, the prompt, and a note.
func outcomeHTML(head, body, note string) string {
	out := "<b>" + escapeHTML(head) + "</b>"
	if body != "" {
		out += "\n" + body
	}
	if note != "" {
		out += "\n\n<i>" + escapeHTML(note) + "</i>"
	}
	return out
}

const (
	answeredElsewhere = "Answered elsewhere"
	answeredSomewhere = "Answered in the hub or another app."
)

// settledOutcome says what became of a prompt that the bot did not
// answer: shown is when it was shown. answer finds the answer the hub
// recorded for an approval; nil when it cannot. final is false for an
// approval answered where the hub could not see the choice, whose outcome
// may follow.
func settledOutcome(p hub.PromptInfo, shown, now time.Time, cancelled bool,
	answer func() (hub.ApprovalOutcome, bool)) (head, note string, final bool) {
	if cancelled {
		return "Cancelled", "Hermes stopped waiting for an answer.", true
	}
	if p.Kind != hub.PromptClarify && answer != nil {
		if o, ok := answer(); ok {
			return outcomeHead(o), "", true
		}
	}
	timeout := time.Duration(p.Timeout * float64(time.Second))
	switch {
	case timeout > 0 && now.Sub(shown) >= timeout-slack(timeout):
		return "Expired", "No answer in " + duration(timeout) + ".", true
	case p.Kind == hub.PromptClarify:
		return answeredElsewhere, answeredSomewhere, true
	case answer == nil:
		return answeredElsewhere, answeredSomewhere, false
	}
	return answeredElsewhere, "Answered in another app.", false
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
	if len(p.Choices) > maxButtons {
		var row []InlineButton
		for i := range p.Choices {
			row = append(row, InlineButton{Text: strconv.Itoa(i + 1), CallbackData: cb(strconv.Itoa(i))})
			if len(row) == 5 {
				rows, row = append(rows, row), nil
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	} else {
		for i, c := range p.Choices {
			label := truncate(c, 40)
			tok := strconv.Itoa(i)
			if p.Multi {
				tok = "t" + tok
				if i < len(picks) && picks[i] {
					label = "✓ " + label
				}
			}
			rows = append(rows, []InlineButton{{Text: label, CallbackData: cb(tok)}})
		}
	}
	last := []InlineButton{}
	if p.Multi {
		last = append(last, InlineButton{Text: "Send", CallbackData: cb(tokSend)})
	}
	other := "Other answer"
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

// SendNotice sends a notice to a person's private chat, or to the General
// topic of their linked group, with buttons when it is a prompt, a button
// that opens the chat's topic when it has one, and a link that opens the
// chat in the hub.
func (b *Bot) SendNotice(ctx context.Context, chatID int64, m notify.Message) (int64, error) {
	rec := m.Record
	if rec.Kind == hub.NoticeClaudeSignIn {
		if id, ok := b.sendClaudeSignIn(ctx, chatID, m); ok {
			return id, nil
		}
	}
	body := noticeHead(m)
	if pb := promptBody(rec.Prompt); pb != "" {
		body += "\n" + pb
	}
	text := body
	if rec.Prompt != nil {
		text += promptFooter(*rec.Prompt)
	}
	if m.URL != "" {
		text += "\n\n<a href=\"" + escapeHTML(m.URL) + "\">Open in the hub</a>"
	}
	kb := &InlineKeyboard{}
	if rec.Prompt != nil {
		kb = promptKeyboard(cbNotice, rec.ID, *rec.Prompt, nil)
	}
	if link := b.topicLink(ctx, chatID, rec.BackendID, rec.SessionID); link != "" {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []InlineButton{{Text: "Open the topic", URL: link}})
	}
	if len(kb.InlineKeyboard) == 0 {
		kb = nil
	}
	id := sendHTML(ctx, b.api, b.logger, dest{chat: chatID}, text, kb)
	if id == 0 {
		return 0, errors.New("telegram: send failed")
	}
	if rec.Prompt == nil {
		return id, nil
	}
	b.mu.Lock()
	b.shown[msgKey{chatID, id}] = shownNotice{body: body, at: b.now()}
	if rec.Prompt.Kind == hub.PromptClarify {
		b.addReplyLocked(msgKey{chatID, id}, replyTarget{prefix: cbNotice, key: rec.ID, question: id})
	}
	b.mu.Unlock()
	if rec.Prompt.Timeout > 0 {
		timeout := time.Duration(rec.Prompt.Timeout * float64(time.Second))
		wait := timeout + slack(timeout)
		if !rec.CreatedAt.IsZero() {
			wait -= b.now().Sub(rec.CreatedAt)
		}
		time.AfterFunc(max(wait, 0), func() { b.expireNotice(chatID, id, rec) })
	}
	return id, nil
}

// expireNotice shows a notice's prompt as expired when nobody answered it
// in its time. It runs from a timer, with no request behind it.
//
//nolint:contextcheck // see above
func (b *Bot) expireNotice(chatID, msgID int64, rec hub.NoticeRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if b.notices != nil {
		if cur, ok, err := b.notices.Notice(ctx, rec.ID); err != nil || ok && !cur.SettledAt.IsZero() {
			return
		}
	}
	b.mu.Lock()
	sn, ok := b.shown[msgKey{chatID, msgID}]
	delete(b.shown, msgKey{chatID, msgID})
	b.dropRepliesLocked(cbNotice, rec.ID)
	b.mu.Unlock()
	if !ok {
		return
	}
	b.editHTML(ctx, chatID, msgID, outcomeHTML("Expired", sn.body,
		"No answer in "+duration(time.Duration(rec.Prompt.Timeout*float64(time.Second)))+"."))
}

// Settled shows the outcome on the messages of a notice whose prompt got
// its answer, or was cancelled, somewhere else, and takes their buttons
// off.
func (b *Bot) Settled(ctx context.Context, rec hub.NoticeRecord, cancelled bool, msgs []hub.NoticeMessage) {
	var answer func() (hub.ApprovalOutcome, bool)
	if _, ok := b.notices.(answerFinder); ok && rec.Prompt != nil && rec.Prompt.Kind != hub.PromptClarify {
		var (
			o      hub.ApprovalOutcome
			found  bool
			looked bool
		)
		answer = func() (hub.ApprovalOutcome, bool) {
			if !looked {
				o, found = b.recordedAnswer(ctx, rec.BackendID, rec.PromptID)
				looked = true
			}
			return o, found
		}
	}
	for _, m := range msgs {
		key := msgKey{m.ChatID, m.MessageID}
		b.mu.Lock()
		sn, ok := b.shown[key]
		b.dropRepliesLocked(cbNotice, rec.ID)
		b.mu.Unlock()
		if rec.Prompt == nil {
			b.forgetShown(key)
			if err := b.api.EditMarkup(ctx, m.ChatID, m.MessageID, nil); err != nil {
				b.logger.Debug("Failed to take buttons off a notice", "chat", m.ChatID, "error", err)
			}
			continue
		}
		if !ok {
			sn = shownNotice{body: noticeBody(rec), at: rec.CreatedAt}
		}
		head, note, final := settledOutcome(*rec.Prompt, sn.at, b.now(), cancelled, answer)
		if final {
			b.forgetShown(key)
		}
		if err := b.api.EditHTML(ctx, m.ChatID, m.MessageID, outcomeHTML(head, sn.body, note), nil); err != nil {
			b.logger.Debug("Failed to show a notice's outcome", "chat", m.ChatID, "error", err)
		}
		if !final {
			b.claimedLate(ctx, rec, m)
		}
	}
}

// claimedFinder is the optional part of a NoticeStore that reads the
// outcome another replica claimed to show.
type claimedFinder interface {
	ClaimedOutcome(ctx context.Context, backendID, promptID string) (hub.ApprovalOutcome, bool, error)
}

// claimedLate shows the outcome of an approval that another replica worked
// out and showed while this one showed it as answered elsewhere, so the
// later edit does not hide the outcome. Either this read sees the claim, or
// the claim comes after this edit, and so does the other replica's edit.
func (b *Bot) claimedLate(ctx context.Context, rec hub.NoticeRecord, m hub.NoticeMessage) {
	cf, ok := b.notices.(claimedFinder)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	o, ok, err := cf.ClaimedOutcome(cctx, rec.BackendID, rec.PromptID)
	if err != nil || !ok {
		return
	}
	b.Outcome(ctx, rec, o, []hub.NoticeMessage{m})
}

// Outcome shows on the messages of a settled approval notice what became
// of the approval, once the hub worked it out from the tool call.
func (b *Bot) Outcome(ctx context.Context, rec hub.NoticeRecord, o hub.ApprovalOutcome, msgs []hub.NoticeMessage) {
	for _, m := range msgs {
		key := msgKey{m.ChatID, m.MessageID}
		b.mu.Lock()
		sn, ok := b.shown[key]
		delete(b.shown, key)
		b.mu.Unlock()
		if !ok {
			sn.body = noticeBody(rec)
		}
		if err := b.api.EditHTML(ctx, m.ChatID, m.MessageID, outcomeHTML(outcomeHead(o), sn.body, ""), nil); err != nil {
			b.logger.Debug("Failed to show an approval's outcome", "chat", m.ChatID, "error", err)
		}
	}
}

// noticeBody is the text of a notice's prompt when the bot no longer has
// the message it sent.
func noticeBody(rec hub.NoticeRecord) string {
	return "<b>" + escapeHTML(notify.Titles(rec.Kind)) + "</b>\n" + promptBody(rec.Prompt)
}

func (b *Bot) forgetShown(key msgKey) {
	b.mu.Lock()
	delete(b.shown, key)
	b.mu.Unlock()
}

// noticeTarget loads the notice that a button or a reply in chat answers.
// Only the person it was sent to may answer, on a message it was sent in.
func (b *Bot) noticeTarget(ctx context.Context, to dest, msgID int64, user, key string) (promptTarget, string) {
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
		found = found || (m.ChatID == to.chat && m.MessageID == msgID)
	}
	if !found {
		return promptTarget{}, msgNotPending
	}
	b.mu.Lock()
	sn, ok := b.shown[msgKey{to.chat, msgID}]
	b.mu.Unlock()
	if !ok {
		sn.body = noticeBody(rec)
	}
	return promptTarget{prefix: cbNotice, key: key, to: to, backendID: rec.BackendID, sessionID: rec.SessionID,
		prompt: *rec.Prompt, body: sn.body, owner: rec.User}, ""
}

// attachedTarget loads the prompt that a button or a reply in an attached
// chat answers.
func (b *Bot) attachedTarget(to dest, msgID int64, key string) (promptTarget, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.approvals[key]
	// messageID is 0 only between the send and its bind; nobody can have
	// seen the message's buttons before then.
	if !ok || p.to.chat != to.chat || p.messageID != msgID {
		return promptTarget{}, msgNotPending
	}
	return promptTarget{prefix: cbApproval, key: key, to: p.to, backendID: p.backendID, sessionID: p.sessionID,
		prompt: p.prompt, body: p.body}, ""
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
		b.askReply(ctx, q, user, t)
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
	b.editHTML(ctx, t.to.chat, q.Message.MessageID, outcomeHTML(truncate(verdict, 300), t.body, "By "+user+"."))
}

// sendAnswer sends a to Hermes as user.
func (b *Bot) sendAnswer(ctx context.Context, user string, t promptTarget, a hub.Answer) error {
	if t.owner != "" && t.owner != user {
		return hub.ErrForbidden
	}
	cctx, cancel := context.WithTimeout(hub.WithAnsweredVia(ctx, hub.AnsweredInTelegram), callTimeout)
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
		b.reply(ctx, callbackID, t.to, msgNotYours)
		return false
	case errors.Is(err, hub.ErrUnknownApproval), errors.Is(err, hub.ErrUnknownSession):
		b.dropPrompt(t)
		b.reply(ctx, callbackID, t.to, msgNotPending)
		return false
	case errors.Is(err, hub.ErrBadValue):
		b.reply(ctx, callbackID, t.to, "Hermes did not take that answer.")
		return false
	case err != nil:
		b.logger.Error("Failed to answer a prompt", "user", user, "error", err)
		b.reply(ctx, callbackID, t.to, errGeneric)
		return false
	}
	b.dropPrompt(t)
	b.answeredHere(ctx, t)
	return true
}

// answeredHere marks a notice that the bot answered settled, so the
// settle that follows does not show it as answered elsewhere.
func (b *Bot) answeredHere(ctx context.Context, t promptTarget) {
	s, ok := b.notices.(noticeSettler)
	if t.prefix != cbNotice || !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := s.SettleNotices(cctx, t.backendID, t.sessionID, []string{t.prompt.ID}); err != nil {
		b.logger.Warn("Failed to mark a notice answered", "error", err)
	}
}

// noticeSettler is the optional part of a NoticeStore that marks notices
// settled.
type noticeSettler interface {
	SettleNotices(ctx context.Context, backendID, sessionID string, promptIDs []string) ([]hub.NoticeRecord, error)
}

// reportAnswerErr tells a chat why its typed answer failed.
func (b *Bot) reportAnswerErr(ctx context.Context, to dest, user string, t promptTarget, err error) {
	switch {
	case errors.Is(err, hub.ErrForbidden):
		b.logger.Warn("Prompt answer forbidden", "user", user, "backend", t.backendID)
		b.sendText(ctx, to, msgNotYours)
	case errors.Is(err, hub.ErrBadValue):
		b.sendText(ctx, to, "Hermes did not take that answer.")
	default:
		b.logger.Error("Failed to answer a prompt", "user", user, "error", err)
		b.sendText(ctx, to, errGeneric)
	}
}

// reply answers a button press, or, for a typed answer, sends text.
func (b *Bot) reply(ctx context.Context, callbackID string, to dest, text string) {
	if callbackID != "" {
		b.answer(ctx, callbackID, text)
		return
	}
	b.sendText(ctx, to, text)
}

func (b *Bot) dropPrompt(t promptTarget) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.picks, t.id())
	b.dropRepliesLocked(t.prefix, t.key)
	if t.prefix == cbApproval {
		delete(b.approvals, t.key)
	}
}

// addReplyLocked makes a reply to the message k answer a prompt, and
// forgets replies older than keepShown. b.mu is held.
func (b *Bot) addReplyLocked(k msgKey, rt replyTarget) {
	now := b.now()
	rt.at = now
	b.replies[k] = rt
	for mk, r := range b.replies {
		if now.Sub(r.at) > keepShown {
			delete(b.replies, mk)
		}
	}
	for mk, sn := range b.shown {
		if now.Sub(sn.at) > keepShown {
			delete(b.shown, mk)
		}
	}
}

// dropRepliesLocked forgets the replies that answer a prompt. b.mu is
// held.
func (b *Bot) dropRepliesLocked(prefix, key string) {
	for mk, r := range b.replies {
		if r.prefix == prefix && r.key == key {
			delete(b.replies, mk)
		}
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
	if err := b.api.EditMarkup(ctx, t.to.chat, q.Message.MessageID, promptKeyboard(t.prefix, t.key, t.prompt, shown)); err != nil {
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

// noticeFinder is the optional part of a NoticeStore that finds the
// notice a message shows.
type noticeFinder interface {
	NoticeForMessage(ctx context.Context, chatID, messageID int64) (string, bool, error)
}

// storedReply finds a question notice by its message in the store, for a
// notice that another replica sent, or one sent before the bot restarted.
func (b *Bot) storedReply(ctx context.Context, chatID, msgID int64) (replyTarget, bool) {
	nf, ok := b.notices.(noticeFinder)
	if !ok {
		return replyTarget{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	id, ok, err := nf.NoticeForMessage(cctx, chatID, msgID)
	if err != nil || !ok {
		return replyTarget{}, false
	}
	rec, ok, err := b.notices.Notice(cctx, id)
	if err != nil || !ok || rec.Prompt == nil || rec.Prompt.Kind != hub.PromptClarify {
		return replyTarget{}, false
	}
	return replyTarget{prefix: cbNotice, key: id, question: msgID}, true
}

// askReply sends a message that opens a reply in the person's app; the
// reply answers the question.
func (b *Bot) askReply(ctx context.Context, q *CallbackQuery, user string, t promptTarget) {
	if !b.mayAnswer(ctx, user, t) {
		b.answer(ctx, q.ID, msgNotYours)
		return
	}
	b.answer(ctx, q.ID, "")
	to := dest{chat: q.Message.Chat.ID}
	if q.Message.IsTopicMessage {
		to.thread = q.Message.MessageThreadID
	}
	question := t.prompt.Question
	if question == "" {
		question = "the question above"
	}
	id, err := b.api.SendForceReply(ctx, to.chat, to.thread, "Reply to this message with your answer to: <i>"+
		escapeHTML(truncate(question, 300))+"</i>", "Your answer")
	if err != nil || id == 0 {
		b.logger.Warn("Failed to ask for an answer", "error", err)
		return
	}
	b.mu.Lock()
	b.addReplyLocked(msgKey{to.chat, id}, replyTarget{prefix: t.prefix, key: t.key, question: q.Message.MessageID})
	b.mu.Unlock()
}

// replyAnswer answers the question whose message m replies to, and
// reports whether m was such an answer. A reply of /skip skips it.
func (b *Bot) replyAnswer(ctx context.Context, to dest, user string, m *Message) bool {
	if m.ReplyToMessage == nil || m.Text == "" {
		return false
	}
	if cmd, _ := parseCommand(m.Text); cmd != "" && cmd != cmdSkip {
		return false
	}
	b.mu.Lock()
	rt, ok := b.replies[msgKey{to.chat, m.ReplyToMessage.MessageID}]
	b.mu.Unlock()
	if !ok {
		rt, ok = b.storedReply(ctx, to.chat, m.ReplyToMessage.MessageID)
	}
	if !ok {
		return false
	}
	var (
		t       promptTarget
		refusal string
	)
	if rt.prefix == cbNotice {
		t, refusal = b.noticeTarget(ctx, to, rt.question, user, rt.key)
	} else {
		t, refusal = b.attachedTarget(to, rt.question, rt.key)
	}
	switch {
	case refusal == msgNotPending:
		b.mu.Lock()
		b.dropRepliesLocked(rt.prefix, rt.key)
		b.mu.Unlock()
		b.sendText(ctx, to, "That question no longer waits for an answer, so your message goes to the chat.")
		return false
	case refusal != "":
		b.sendText(ctx, to, refusal)
		return true
	case !b.mayAnswer(ctx, user, t):
		b.sendText(ctx, to, msgNotYours)
		return true
	}
	a, verdict := hub.Answer{Text: m.Text}, "Answered: "+m.Text
	if cmd, _ := parseCommand(m.Text); cmd == cmdSkip {
		a, verdict = hub.Answer{Skip: true}, "Skipped"
	}
	err := b.sendAnswer(ctx, user, t, a)
	if errors.Is(err, hub.ErrUnknownApproval) || errors.Is(err, hub.ErrUnknownSession) {
		b.dropPrompt(t)
		b.sendText(ctx, to, "That question no longer waits for an answer, so your message goes to the chat.")
		return false
	}
	if err != nil {
		b.reportAnswerErr(ctx, to, user, t, err)
		return true
	}
	b.dropPrompt(t)
	b.answeredHere(ctx, t)
	b.editHTML(ctx, to.chat, rt.question, outcomeHTML(truncate(verdict, 300), t.body, "By "+user+"."))
	return true
}
