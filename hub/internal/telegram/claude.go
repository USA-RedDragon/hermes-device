package telegram

import (
	"context"
	"errors"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/notify"
)

const (
	// claudePoll is how often the bot reads a sign-in that verifies its
	// code, and claudeVerify how long it waits for the result.
	claudePoll   = 2 * time.Second
	claudeVerify = time.Minute
	// claudeTimeout bounds a sign-in call; sending the code may take the
	// plugin 90 seconds.
	claudeTimeout = 2 * time.Minute
)

// claudePrompt is a Claude sign-in message whose reply is the code.
type claudePrompt struct {
	user      string
	backendID string
	account   string
	body      string
	at        time.Time
}

// claudeHead is the first line of a Claude sign-in message.
const claudeHead = "<b>A Claude account needs a sign-in</b>"

// accountLine names the account of a sign-in: its label and the email it
// was last signed in as.
func accountLine(si hub.ClaudeSignIn) string {
	label := si.Label
	if label == "" {
		label = si.Account
	}
	if si.EmailHint != "" {
		return label + ", last signed in as " + si.EmailHint
	}
	return label + ", first sign-in"
}

// sendClaudeSignIn starts the sign-in of a notice's Claude account as its
// admin and sends the admin its link, with a reply box for the code. ok is
// false when it could not start one, so the caller sends the plain notice.
func (b *Bot) sendClaudeSignIn(ctx context.Context, chatID int64, m notify.Message) (int64, bool) {
	rec := m.Record
	if b.claude == nil || rec.Account == "" || rec.User == "" {
		return 0, false
	}
	cctx, cancel := context.WithTimeout(ctx, claudeTimeout)
	defer cancel()
	si, err := b.claude.StartSignIn(cctx, rec.User, rec.BackendID, rec.Account)
	if err != nil || si.URL == "" {
		b.logger.Warn("Failed to start a Claude sign-in for Telegram", "backend", rec.BackendID, "account", rec.Account, "error", err)
		return 0, false
	}
	body := claudeHead
	if m.Backend != "" {
		body += " on <b>" + escapeHTML(m.Backend) + "</b>"
	}
	body += "\n" + escapeHTML(accountLine(si))
	text := body + "\n\n<a href=\"" + escapeHTML(si.URL) + "\">Open the Claude sign-in page</a>, sign in, " +
		"then reply to this message with the code that the page shows."
	id, err := b.api.SendForceReply(cctx, chatID, 0, text, "Code from the Claude page")
	if err != nil || id == 0 {
		b.logger.Warn("Failed to send a Claude sign-in link", "error", err)
		return 0, false
	}
	b.mu.Lock()
	b.claudes[msgKey{chatID, id}] = claudePrompt{user: rec.User, backendID: rec.BackendID, account: rec.Account, body: body, at: b.now()}
	b.mu.Unlock()
	if !si.ExpiresAt.IsZero() {
		left := si.ExpiresAt.Sub(b.now())
		time.AfterFunc(max(left+slack(left), 0), func() { b.expireClaude(msgKey{chatID, id}) })
	}
	return id, true
}

// storedClaude finds the Claude sign-in notice a message shows in the
// store, for one that another replica sent or one sent before the bot
// restarted.
func (b *Bot) storedClaude(ctx context.Context, k msgKey) (claudePrompt, bool) {
	nf, ok := b.notices.(noticeFinder)
	if !ok || b.claude == nil {
		return claudePrompt{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	id, ok, err := nf.NoticeForMessage(cctx, k.chat, k.msg)
	if err != nil || !ok {
		return claudePrompt{}, false
	}
	rec, ok, err := b.notices.Notice(cctx, id)
	if err != nil || !ok || rec.Kind != hub.NoticeClaudeSignIn || rec.Account == "" {
		return claudePrompt{}, false
	}
	return claudePrompt{user: rec.User, backendID: rec.BackendID, account: rec.Account, body: claudeHead, at: rec.CreatedAt}, true
}

// claudeReply takes m as the code of the Claude sign-in whose message it
// replies to, and reports whether it did. Only the admin the link went to
// may send the code. The code goes to the sign-in and nowhere else: the
// bot does not log or keep it, and deletes the message when it may.
func (b *Bot) claudeReply(ctx context.Context, to dest, user string, m *Message) bool {
	if m.ReplyToMessage == nil || m.Text == "" {
		return false
	}
	k := msgKey{to.chat, m.ReplyToMessage.MessageID}
	b.mu.Lock()
	cp, ok := b.claudes[k]
	b.mu.Unlock()
	if !ok {
		cp, ok = b.storedClaude(ctx, k)
	}
	if !ok {
		return false
	}
	if cmd, _ := parseCommand(m.Text); cmd != "" {
		return false
	}
	if cp.user != user {
		b.logger.Warn("Claude sign-in code from another user", "user", user)
		b.sendText(ctx, to, "Only the person this sign-in was sent to may send its code.")
		return true
	}
	code, msgID := m.Text, m.MessageID
	go b.sendClaudeCode(context.WithoutCancel(ctx), to, k, msgID, cp, code)
	return true
}

// sendClaudeCode passes code to the account's running sign-in and shows
// the result in the sign-in's message.
func (b *Bot) sendClaudeCode(ctx context.Context, to dest, k msgKey, codeMsg int64, cp claudePrompt, code string) {
	cctx, cancel := context.WithTimeout(ctx, claudeTimeout+claudeVerify)
	defer cancel()
	si, err := b.claude.SignInState(cctx, cp.user, cp.backendID, cp.account)
	if err == nil && si.State == hub.SignInWaiting {
		si, err = b.claude.SendSignInCode(cctx, cp.user, cp.backendID, cp.account, si.ID, code)
	}
	if err := b.api.DeleteMessage(cctx, to.chat, codeMsg); err != nil {
		b.logger.Debug("Could not delete a Claude sign-in code message", "error", err)
	}
	if errors.Is(err, hub.ErrBadCode) {
		b.sendText(ctx, to, "That code was not accepted. Copy the whole code from the Claude page and reply to the sign-in message again.")
		return
	}
	for deadline := time.Now().Add(claudeVerify); err == nil && (si.State == hub.SignInVerifying || si.State == hub.SignInStarting) &&
		time.Now().Before(deadline); {
		if !sleep(cctx, claudePoll) {
			break
		}
		si, err = b.claude.SignInState(cctx, cp.user, cp.backendID, cp.account)
	}
	b.claudeOutcome(cctx, to, k, cp, si, err)
}

// claudeOutcome shows what became of a sign-in in its message. A sign-in
// that still waits for a code keeps its message.
func (b *Bot) claudeOutcome(ctx context.Context, to dest, k msgKey, cp claudePrompt, si hub.ClaudeSignIn, err error) {
	var head, note string
	switch {
	case errors.Is(err, hub.ErrNoSignIn), err == nil && (si.State == hub.SignInTimedOut || si.State == hub.SignInCancelled):
		head, note = "Expired", "The sign-in ended before it got a code. Open the hub to start another."
	case err != nil:
		head, note = "Sign-in failed", err.Error()
	case si.State == hub.SignInSucceeded:
		head = "Signed in"
		if si.Email != "" {
			head += " as " + si.Email
		}
	case si.State == hub.SignInFailed:
		head, note = "Sign-in failed", si.Message
	case si.State == hub.SignInWaiting:
		b.sendText(ctx, to, "Claude did not take that code. Reply to the sign-in message again with the whole code.")
		return
	default:
		head, note = "Sign-in still running", "Open the hub to see how it ends."
	}
	b.mu.Lock()
	delete(b.claudes, k)
	b.mu.Unlock()
	b.editHTML(ctx, k.chat, k.msg, outcomeHTML(head, cp.body, note))
}

// expireClaude shows a sign-in whose link expired as expired, unless it
// ended otherwise. It runs from a timer, with no request behind it.
//
//nolint:contextcheck // see above
func (b *Bot) expireClaude(k msgKey) {
	b.mu.Lock()
	cp, ok := b.claudes[k]
	b.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	si, err := b.claude.SignInState(ctx, cp.user, cp.backendID, cp.account)
	if err == nil && (si.State == hub.SignInWaiting || si.State == hub.SignInStarting) {
		si.State = hub.SignInTimedOut
	}
	if err == nil && si.State == hub.SignInVerifying {
		return
	}
	b.claudeOutcome(ctx, dest{chat: k.chat}, k, cp, si, err)
}
