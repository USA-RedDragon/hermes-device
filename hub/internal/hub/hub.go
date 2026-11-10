// Package hub defines the contract between the hub core and its front ends
// (web UI, Telegram bot).
package hub

import (
	"context"
	"encoding/json"
	"time"
)

// Backend is one Hermes gateway a user may reach.
type Backend struct {
	ID, Name string
}

// Session is one Hermes session on a backend.
type Session struct {
	ID, BackendID, Title string
	Live, Running        bool
	// Archived sessions are hidden from the gateway's normal list.
	Archived     bool
	MessageCount int
	UpdatedAt    time.Time
}

// Event kinds delivered on an attached session.
const (
	KindReasoningDelta  = "reasoning.delta"
	KindThinkingDelta   = "thinking.delta"
	KindToolStart       = "tool.start"
	KindToolProgress    = "tool.progress"
	KindToolComplete    = "tool.complete"
	KindMessageStart    = "message.start"
	KindMessageDelta    = "message.delta"
	KindMessageComplete = "message.complete"
	KindApprovalRequest = "approval.request"
	KindError           = "error"
)

// Event is one ordered event from an attached session. Data is the gateway
// payload for that event, passed through unchanged.
type Event struct {
	Seq  int64
	Kind string
	Data json.RawMessage
}

// Service is the hub core. user is always the Authentik username; every
// method fails with ErrForbidden when the backend is not mapped to user.
type Service interface {
	Backends(ctx context.Context, user string) ([]Backend, error)
	Sessions(ctx context.Context, user string) ([]Session, error)
	NewSession(ctx context.Context, user, backendID string) (Session, error)
	// Attach streams events after sinceSeq (0 for a fresh attach). The
	// returned func detaches and closes the channel.
	Attach(ctx context.Context, user, backendID, sessionID string, sinceSeq int64) (<-chan Event, func(), error)
	// Prompt submits text as the session's next input. While the backend
	// restarts or cannot be reached, the hub holds it and submits it once
	// the backend is back: the result says so.
	Prompt(ctx context.Context, user, backendID, sessionID, text string) (PromptResult, error)
	Interrupt(ctx context.Context, user, backendID, sessionID string) error
	RespondApproval(ctx context.Context, user, backendID, sessionID, approvalID string, allow bool) error
	// CancelInput drops a queued input before it runs.
	CancelInput(ctx context.Context, user, backendID, sessionID, inputID string) error
	// DismissLost resolves a lost input so the inputs queued after it run.
	DismissLost(ctx context.Context, user, backendID, sessionID, inputID string) error
	// RenameSession sets a session's title.
	RenameSession(ctx context.Context, user, backendID, sessionID, title string) error
	// ArchiveSession hides a session from the list, or shows it again.
	ArchiveSession(ctx context.Context, user, backendID, sessionID string, archived bool) error
	// DeleteSession deletes a session for good. It fails with ErrBusy
	// while the session runs or has inputs waiting.
	DeleteSession(ctx context.Context, user, backendID, sessionID string) error
	// Models lists the models the session's backend can use.
	Models(ctx context.Context, user, backendID, sessionID string) (Models, error)
	// SetModel picks the session's model. confirm is the Confirm of an
	// earlier result that asked for confirmation.
	SetModel(ctx context.Context, user, backendID, sessionID, provider, model, confirm string) (ModelChange, error)
	// SetReasoning sets the session's reasoning effort: one of
	// ReasoningEfforts, or "" for the backend's default.
	SetReasoning(ctx context.Context, user, backendID, sessionID, effort string) error
}

// Models is the model choice of one session.
type Models struct {
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	Providers []ModelProvider `json:"providers"`
}

// ModelProvider is one provider and the models it offers.
type ModelProvider struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Models []ModelChoice `json:"models"`
}

// ModelChoice is one model. Reasoning is false when the model has no
// reasoning effort to set; CanDisableReasoning is false when its reasoning
// cannot be turned off.
type ModelChoice struct {
	ID                  string `json:"id"`
	Reasoning           bool   `json:"reasoning"`
	CanDisableReasoning bool   `json:"can_disable_reasoning"`
}

// Model change outcomes.
const (
	ModelChanged   = "changed"
	ModelDeferred  = "deferred"
	ModelUnchanged = "unchanged"
	ModelConfirm   = "confirm"
)

// ModelChange is the outcome of SetModel. With Status ModelConfirm nothing
// changed: Message says why, and SetModel with Confirm applies it.
type ModelChange struct {
	Status  string `json:"status"`
	Confirm string `json:"confirm,omitempty"`
	Message string `json:"message,omitempty"`
}

// ReasoningEfforts are the reasoning efforts SetReasoning takes besides
// "" (the backend's default). "none" turns reasoning off.
func ReasoningEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

// PromptResult is what became of a prompt. ID is the submission ID the
// hub sent it under, or will send it under. Held is true when the backend
// is restarting or unreachable: the hub submits the prompt when the backend
// is back, or reports it with KindInputNotSent at HeldUntil.
type PromptResult struct {
	ID        string
	Held      bool
	HeldUntil time.Time
}

// Links maps external identities to hub users.
type Links interface {
	UserForTelegram(ctx context.Context, telegramID int64) (string, bool, error)
	LinkTelegram(ctx context.Context, telegramID int64, user string) error
}

// TokenStore issues one-time tokens that bind a Telegram account to the
// hub user who opens /link/telegram?token=<token> while logged in.
type TokenStore interface {
	CreateLinkToken(ctx context.Context, telegramID int64) (string, error)
	// ConsumeLinkToken returns the token's Telegram ID and deletes the
	// token. ok is false for an unknown, used or expired token.
	ConsumeLinkToken(ctx context.Context, token string) (telegramID int64, ok bool, err error)
}

// Event kinds the hub adds to the gateway's own.
const (
	// KindSnapshot starts a stream and replaces everything a viewer shows:
	// Data is a Snapshot. It is sent on a fresh attach, when sinceSeq is no
	// longer in the hub's buffer, and after the backend lost the stream.
	KindSnapshot = "snapshot"
	// KindBackendStatus reports the backend connection: Data is
	// {"connected": bool}.
	KindBackendStatus = "backend.status"
	// KindSessionDeleted ends the stream of a deleted session.
	KindSessionDeleted = "session.deleted"
	// KindInputHeld reports a prompt the hub holds while the backend
	// restarts: Data is a HeldInput.
	KindInputHeld = "input.held"
	// KindInputSent reports that the backend took a held prompt: Data is a
	// HeldInput without Text.
	KindInputSent = "input.sent"
	// KindInputNotSent reports a held prompt the hub gave up on: Data is a
	// HeldInput with Reason and Message.
	KindInputNotSent = "input.not_sent"
)

// Reasons a held prompt was not sent.
const (
	NotSentTimeout  = "timeout"
	NotSentRefused  = "refused"
	NotSentShutdown = "shutdown"
)

// HeldInput is the Data of the input.* events. Text may be cut short when
// the event came from another hub replica.
type HeldInput struct {
	ID        string    `json:"id"`
	Text      string    `json:"text,omitempty"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// TelegramChat is the session a Telegram chat is attached to.
type TelegramChat struct {
	ChatID    int64
	User      string
	BackendID string
	SessionID string
}

// TelegramState keeps the Telegram bot's chat attachments and update
// offset, so the hub replica that takes the bot over carries on where the
// last one stopped.
type TelegramState interface {
	SaveTelegramChat(ctx context.Context, c TelegramChat) error
	TelegramChats(ctx context.Context) ([]TelegramChat, error)
	TelegramOffset(ctx context.Context, botID int64) (int64, error)
	SetTelegramOffset(ctx context.Context, botID, offset int64) error
}

// Snapshot is the Data of a KindSnapshot event. Messages and Prompts are
// the gateway's transcript rows and pending prompts, passed through.
type Snapshot struct {
	SessionID string            `json:"session_id"`
	BackendID string            `json:"backend_id"`
	Title     string            `json:"title"`
	Running   bool              `json:"running"`
	Messages  []json.RawMessage `json:"messages"`
	Prompts   []json.RawMessage `json:"prompts"`
	Pending   []Pending         `json:"pending"`
	Info      Info              `json:"info"`
}

// Info is a session's settings and its latest usage, for a status line.
// Usage is the gateway's usage object, passed through: model,
// context_used, context_max, context_percent, avg_tps and more, each only
// when known.
type Info struct {
	Model           string          `json:"model,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	CWD             string          `json:"cwd,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ContextLength   int64           `json:"context_length,omitempty"`
	Usage           json.RawMessage `json:"usage,omitempty"`
}

// Pending is an input that has not run yet: "queued" behind the running
// turn, or "lost" when its turn was cut off by a backend restart. A lost
// input holds back everything queued after it until it is dismissed.
// Pending statuses.
const (
	PendingQueued = "queued"
	PendingLost   = "lost"
)

type Pending struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Text   string `json:"text"`
}
