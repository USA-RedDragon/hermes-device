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
	// UpdatedAt is when the session last changed, or when it started when
	// the backend did not say.
	UpdatedAt time.Time
	// Source is the surface that started the session, such as "gui" (the
	// hub or the web app), "cli", "tui" or "acp" (an editor such as VS
	// Code). "" when the backend did not say.
	Source string
	// Activity counts the subagents and background commands the session
	// runs now.
	Activity int
	// Needs is NeedsApproval or NeedsQuestion while the session waits for
	// its user, as far as this hub replica knows; "" otherwise.
	Needs string
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
	KindMessageInterim  = "message.interim"
	KindApprovalRequest = "approval.request"
	// KindPromptLost is a hub-made event: Hermes no longer lists these
	// pending prompts (data: prompt_ids) and the hub saw no answer to them.
	KindPromptLost = "prompt.lost"
	KindError      = "error"
)

// Event is one ordered event from an attached session. Data is the gateway
// payload for that event, passed through unchanged. At is when the hub got
// the event, in Unix milliseconds; it is 0 for an event of a turn that ran
// before the hub subscribed, whose time is not known.
type Event struct {
	Seq  int64
	Kind string
	Data json.RawMessage
	At   int64
	// Provisional marks a held snapshot sent while a fresh one is on its
	// way: something to show at once, which the fresh one replaces. It
	// has no place in the stream (Seq 0).
	Provisional bool
}

// Service is the hub core. user is always the Authentik username; every
// method fails with ErrForbidden when the backend is not mapped to user.
type Service interface {
	Backends(ctx context.Context, user string) ([]Backend, error)
	// Sessions lists the user's chats on every backend. When some backends
	// could not be listed it returns the rows it has together with an
	// *UnreachableError.
	Sessions(ctx context.Context, user string) ([]Session, error)
	NewSession(ctx context.Context, user, backendID string) (Session, error)
	// Attach streams events after sinceSeq (0 for a fresh attach). With
	// SinceNow it streams a snapshot, the events of the turn that runs now,
	// if any, and what comes after. The returned func detaches and closes
	// the channel.
	Attach(ctx context.Context, user, backendID, sessionID string, sinceSeq int64) (<-chan Event, func(), error)
	// Prompt submits text as the session's next input. While the backend
	// restarts or cannot be reached, the hub holds it and submits it once
	// the backend is back: the result says so. Images among files go to
	// the model; other files go to the device that runs the session, or to
	// Hermes when no device runs it, and the text names where they are.
	// Prompt fails with ErrAttachment when files break the limits.
	Prompt(ctx context.Context, user, backendID, sessionID, text string, files ...Attachment) (PromptResult, error)
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

// NewChatModels is the model choice of a chat that does not exist yet: the
// backend's default model and provider, every model it offers, and the
// default model's context window in tokens (0 when unknown).
type NewChatModels struct {
	Models
	ContextLength int64 `json:"context_length,omitempty"`
}

// ModelProvider is one provider and the models it offers. Group is the
// heading the pickers show it under; providers with the same group share
// one heading.
type ModelProvider struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Group  string        `json:"group"`
	Models []ModelChoice `json:"models"`
}

// ModelChoice is one model. Label is what the pickers show for it.
// Reasoning is false when the model has no reasoning effort to set;
// CanDisableReasoning is false when its reasoning cannot be turned off.
type ModelChoice struct {
	ID                  string `json:"id"`
	Label               string `json:"label"`
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
	// Notice is a note for the user about how the prompt went, such as
	// images the session's model cannot read.
	Notice string
	// Steered is true when the running turn took the prompt's text (see
	// WithSteer).
	Steered bool
	// Command is true for a slash command that Hermes ran at once instead
	// of a prompt: Output is what it answered, and ID is "".
	Command bool
	Output  string
}

type submissionKey struct{}

// WithSubmissionID makes Prompt send its prompt under id, instead of a new
// one. A front end that may hand the same input to Prompt twice, as the
// Telegram bot can when another replica takes it over, derives id from the
// input, so Hermes runs it once.
func WithSubmissionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, submissionKey{}, id)
}

// SubmissionID is the ID WithSubmissionID set, or "".
func SubmissionID(ctx context.Context) string {
	id, _ := ctx.Value(submissionKey{}).(string)
	return id
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

// SinceNow is the sinceSeq of an Attach that skips the turns that ended
// before it.
const SinceNow int64 = -1

// TelegramGroup is the forum group a hub user linked to the Telegram bot,
// or, with a positive ChatID, the user's private chat with the bot when
// the bot has topics in private chats and the user linked no group. Each
// of the user's chats gets a topic there.
type TelegramGroup struct {
	User     string
	ChatID   int64
	LinkedAt time.Time
}

// TelegramTopic is the forum topic of one chat. ThreadID 0 marks a topic
// that was deleted or closed in Telegram: the chat gets a new one when it
// runs again a while after Seen.
type TelegramTopic struct {
	ChatID    int64
	ThreadID  int64
	User      string
	BackendID string
	SessionID string
	// Name is the topic's name as the bot last set it.
	Name string
	// Closed is a topic the bot closed because its chat is archived.
	Closed bool
	// Seen is when the bot made or dropped the topic.
	Seen time.Time
}

// TelegramState keeps the Telegram bot's chat attachments, linked groups,
// topics and update offset, so the hub replica that takes the bot over
// carries on where the last one stopped.
type TelegramState interface {
	SaveTelegramChat(ctx context.Context, c TelegramChat) error
	TelegramChats(ctx context.Context) ([]TelegramChat, error)
	TelegramOffset(ctx context.Context, botID int64) (int64, error)
	SetTelegramOffset(ctx context.Context, botID, offset int64) error
	// LinkTelegramGroup links g.ChatID to g.User. It replaces the user's
	// earlier group and forgets its topics.
	LinkTelegramGroup(ctx context.Context, g TelegramGroup) error
	// UnlinkTelegramGroup forgets a group and its topics.
	UnlinkTelegramGroup(ctx context.Context, chatID int64) error
	TelegramGroups(ctx context.Context) ([]TelegramGroup, error)
	// SaveTelegramTopic records the topic of a chat in a group.
	SaveTelegramTopic(ctx context.Context, t TelegramTopic) error
	// TelegramTopics lists the topics of a group.
	TelegramTopics(ctx context.Context, chatID int64) ([]TelegramTopic, error)
	// DeleteTelegramTopic forgets the topic of a chat in a group.
	DeleteTelegramTopic(ctx context.Context, chatID int64, backendID, sessionID string) error
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
	// ToolBackground is true when the backend can move a running terminal
	// command to the background (Activity.BackgroundTool).
	ToolBackground bool `json:"tool_background,omitempty"`
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
// turn, "steering" while Hermes hands it to the running turn, or "lost"
// when its turn was cut off by a backend restart. A lost input holds back
// everything queued after it until it is dismissed.
// Pending statuses.
const (
	PendingQueued   = "queued"
	PendingSteering = "steering"
	PendingLost     = "lost"
)

type Pending struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Text   string `json:"text"`
	// SubmissionID is the ID the sender gave the input, "" when it has
	// none. A page that sent it knows it by this ID.
	SubmissionID string `json:"submission_id,omitempty"`
}

// KindClarifyRequest is a question the agent asks the user. Its Data has
// the shape of a PromptInfo.
const KindClarifyRequest = "clarify.request"

// Prompt kinds.
const (
	PromptApproval = "approval"
	PromptClarify  = "clarify"
)

// Approval choices.
const (
	ChoiceOnce    = "once"
	ChoiceSession = "session"
	ChoiceAlways  = "always"
	ChoiceDeny    = "deny"
)

// PromptInfo is a pending approval or question. Choices are the approval
// choices the gateway offers (once, deny, session, always), or the
// answers a question offers; a question without choices takes free text.
type PromptInfo struct {
	ID          string   `json:"prompt_id"`
	Kind        string   `json:"kind"`
	Choices     []string `json:"choices"`
	Multi       bool     `json:"multi_select,omitempty"`
	Command     string   `json:"command,omitempty"`
	Description string   `json:"description,omitempty"`
	// Subagent is the goal of the subagent that asks an approval, empty
	// when the chat itself does.
	Subagent string `json:"subagent,omitempty"`
	Question string `json:"question,omitempty"`
	// Timeout is how many seconds Hermes waits for the answer, 0 when
	// it did not say.
	Timeout float64 `json:"timeout_s,omitempty"`
}

// Answer answers a PromptInfo. For an approval Choice is one of its
// Choices. For a question Text is free text or one of its Choices, Picks
// are the picked Choices of a multi-select question, and Skip skips it.
type Answer struct {
	Choice string   `json:"choice,omitempty"`
	Text   string   `json:"text,omitempty"`
	Picks  []string `json:"picks,omitempty"`
	Skip   bool     `json:"skip,omitempty"`
}

// Prompter is the optional part of a Service that answers approvals and
// questions with every choice the gateway offers.
type Prompter interface {
	// PendingPrompt returns a prompt that still waits for an answer, or
	// ErrUnknownApproval.
	PendingPrompt(ctx context.Context, user, backendID, sessionID, promptID string) (PromptInfo, error)
	// AnswerPrompt answers a pending prompt. It fails with ErrBadValue for
	// an answer the prompt does not take.
	AnswerPrompt(ctx context.Context, user, backendID, sessionID, promptID string, a Answer) error
}

// Notice kinds: what a person can be told about while they are away.
const (
	NoticeApproval = "approval"
	NoticeQuestion = "question"
	NoticeFinished = "finished"
	NoticeJob      = "job"
	NoticeFailure  = "failure"
	// NoticeClaudeSignIn says a Claude account that a chat uses lost its
	// login.
	NoticeClaudeSignIn = "claude_signin"
	// NoticeSettled says a prompt got its answer or was cancelled.
	NoticeSettled = "settled"
)

// Notice is something that happened on a backend that its users may want
// to hear about. Title and Detail may carry chat content: front ends that
// leave the hub (Web Push) must not send them.
type Notice struct {
	Kind      string
	BackendID string
	SessionID string
	// Key is unique per occurrence, so the same notice seen twice (by two
	// replicas, or again after a reconnect) is sent once.
	Key    string
	Prompt *PromptInfo
	// PromptIDs are the prompts a NoticeSettled settles.
	PromptIDs []string
	// Title is the chat's title, "" when unknown.
	Title string
	// Detail is a short line about what happened, such as a job's name
	// or why it failed.
	Detail string
	// Users limits the notice to these hub users; nil is every user of
	// the backend.
	Users []string
	// Cancelled marks a NoticeSettled of prompts that were cancelled, not
	// answered.
	Cancelled bool
	// Account is the Claude account of a NoticeClaudeSignIn.
	Account string
	// Outcome is what became of the approval of a NoticeOutcome, or of a
	// NoticeSettled whose answer the hub recorded.
	Outcome *ApprovalOutcome
	// Results is where a scheduled job's results go (ResultsHub and so
	// on), for a job notice; "" follows the person's settings.
	Results string
}

// SessionChecker is the optional part of a Service that tells whether a
// session still exists on its backend.
type SessionChecker interface {
	// SessionExists is false for a session the backend does not know,
	// such as a deleted one. An error means it could not tell.
	SessionExists(ctx context.Context, user, backendID, sessionID string) (bool, error)
}
