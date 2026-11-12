package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
)

// What a session waits for from its user, in Session.Needs.
const (
	NeedsApproval = "approval"
	NeedsQuestion = "question"
)

type steerKey struct{}

// WithSteer makes Prompt hand its text to the session's running turn when
// the prompt queues behind it. The prompt stays an ordinary queued prompt
// when the turn cannot take it.
func WithSteer(ctx context.Context) context.Context {
	return context.WithValue(ctx, steerKey{}, true)
}

// Steer reports whether WithSteer is set on ctx.
func Steer(ctx context.Context) bool {
	v, _ := ctx.Value(steerKey{}).(bool)
	return v
}

// ErrNoUserMessage is a rewind of a session with no message of the user.
var ErrNoUserMessage = errors.New("the chat has no message of yours to go back to")

// ErrUnknownCommand is a slash command that Hermes does not run for the
// hub. Hermes' hint, when it gave one, follows it after ": ".
var ErrUnknownCommand = errors.New("unknown command")

// Rewind is a request to take back the last user message of a session and
// send a new one in its place.
type Rewind struct {
	// Text replaces the message. With Retry and no Text, the message is
	// sent again as it was.
	Text  string
	Retry bool
	// Provider and Model, when Model is set, pick the session's model
	// first; Confirm is the Confirm of an earlier result that asked for
	// confirmation.
	Provider, Model, Confirm string
}

// RewindResult is the outcome of a Rewind. With Model.Status ModelConfirm
// nothing changed and Prompt is empty.
type RewindResult struct {
	Prompt  PromptResult
	Removed int
	Model   *ModelChange
}

// Transcript is a session's stored transcript: the gateway's display rows,
// passed through, oldest first.
type Transcript struct {
	Title    string            `json:"title"`
	Messages []json.RawMessage `json:"messages"`
}

// Command is one slash command a session can run from the hub. Args is a
// hint of its arguments, "" when it takes none or Hermes gave none.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Args        string `json:"args"`
}

// ChatActions is the optional part of a Service that edits a session's
// history and reads it.
type ChatActions interface {
	// Rewind drops the session's last user message and everything after
	// it, then sends r's text. It fails with ErrBusy while a turn runs or
	// inputs wait, and with ErrNoUserMessage when there is nothing to take
	// back.
	Rewind(ctx context.Context, user, backendID, sessionID string, r Rewind) (RewindResult, error)
	// Branch copies a session into a new one, through the row rowID (0 for
	// all of it). It fails with ErrBusy while a turn runs.
	Branch(ctx context.Context, user, backendID, sessionID string, rowID int64, title string) (Session, error)
	// Transcript reads a session's whole stored transcript.
	Transcript(ctx context.Context, user, backendID, sessionID string) (Transcript, error)
	// Commands lists the slash commands a session can run from the hub.
	Commands(ctx context.Context, user, backendID, sessionID string) ([]Command, error)
}

// Redirector is the optional part of a Service that turns a waiting input
// into the next turn.
type Redirector interface {
	// StopAndSend stops the session's running turn and runs inputID, a
	// queued input or one handed to the running turn as a steer, as the
	// next turn, after any inputs queued before it. inputID is an
	// admission ID or the submission ID the input was sent under. It fails
	// with ErrUnknownInput when the input is not waiting, and with ErrBusy
	// when it is the turn that runs.
	StopAndSend(ctx context.Context, user, backendID, sessionID, inputID string) (PromptResult, error)
}

// ChatMark is what a user marked on one chat. SeenAt is zero when the user
// has no read time for it.
type ChatMark struct {
	Pinned bool
	SeenAt time.Time
	// Unread is a chat the user marked unread by hand.
	Unread bool
	// Page is a chat the Firefox extension's page chat started.
	Page bool
}

// ChatStore keeps each user's drafts, pins and read state of chats.
type ChatStore interface {
	// Draft is user's draft of a chat; text is "" when there is none.
	Draft(ctx context.Context, user, backendID, sessionID string) (text string, updated time.Time, err error)
	// SetDraft saves a draft; "" deletes it.
	SetDraft(ctx context.Context, user, backendID, sessionID, text string) error
	SetPinned(ctx context.Context, user, backendID, sessionID string, pinned bool) error
	// MarkRead moves the chat's read time to at, never back and never past
	// now, and clears a hand-set unread mark.
	MarkRead(ctx context.Context, user, backendID, sessionID string, at time.Time) error
	// MarkUnread marks the chat unread by hand.
	MarkUnread(ctx context.Context, user, backendID, sessionID string) error
	// MarkPage marks a chat that the page chat started.
	MarkPage(ctx context.Context, user, backendID, sessionID string) error
	// ChatMarks returns user's marks by chat and the user's read baseline:
	// a chat with no read time counts as read when it last changed before
	// it. The first call for a user sets the baseline to now.
	ChatMarks(ctx context.Context, user string) (map[SessionRef]ChatMark, time.Time, error)
}

// IsUnread reports whether a chat counts as unread: marked unread by hand,
// or idle and changed after the user last saw it (or, never seen, after
// baseline).
func (m ChatMark) IsUnread(s Session, baseline time.Time) bool {
	if m.Unread {
		return true
	}
	if s.Running {
		return false
	}
	seen := m.SeenAt
	if seen.IsZero() {
		seen = baseline
	}
	return s.UpdatedAt.UnixMilli() > seen.UnixMilli()
}

// SlashCommand returns the name and argument of a slash command in text:
// text that starts with "/" and a name, after leading spaces. ok is false
// for other text.
func SlashCommand(text string) (name, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	t = t[1:]
	i := strings.IndexFunc(t, unicode.IsSpace)
	if i < 0 {
		i = len(t)
	}
	name, arg = t[:i], strings.TrimSpace(t[i:])
	if name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return name, arg, true
}
