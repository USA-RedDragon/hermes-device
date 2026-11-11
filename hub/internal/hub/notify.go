package hub

import (
	"context"
	"time"
)

// NotifySettings are one person's choices of what reaches their phone.
// Quiet hours run from QuietStart to QuietEnd ("HH:MM", both empty for
// none) in TimeZone (an IANA name, "" for UTC).
type NotifySettings struct {
	Approvals bool `json:"approvals"`
	Questions bool `json:"questions"`
	Finished  bool `json:"finished"`
	Jobs      bool `json:"jobs"`
	Failures  bool `json:"failures"`
	// ClaudeSignIn is for Claude admins: a Claude account lost its login.
	ClaudeSignIn bool   `json:"claude_signin"`
	Telegram     bool   `json:"telegram"`
	Push         bool   `json:"push"`
	QuietStart   string `json:"quiet_start"`
	QuietEnd     string `json:"quiet_end"`
	TimeZone     string `json:"time_zone"`
}

// DefaultNotifySettings are the settings of a person who never changed
// them: approvals, questions, failures and Claude sign-ins, on Telegram and Web Push.
func DefaultNotifySettings() NotifySettings {
	return NotifySettings{Approvals: true, Questions: true, Failures: true, ClaudeSignIn: true, Telegram: true, Push: true}
}

// Wants reports whether the settings turn on notices of kind.
func (s NotifySettings) Wants(kind string) bool {
	switch kind {
	case NoticeApproval:
		return s.Approvals
	case NoticeQuestion:
		return s.Questions
	case NoticeFinished:
		return s.Finished
	case NoticeJob:
		return s.Jobs
	case NoticeFailure:
		return s.Failures
	case NoticeClaudeSignIn:
		return s.ClaudeSignIn
	}
	return false
}

// NoticeRecord is a notice as the hub stored it for one person: it keeps
// the same notice from going out twice, links Telegram buttons to the
// prompt they answer, and drives reminders.
type NoticeRecord struct {
	ID        string
	User      string
	Key       string
	Kind      string
	BackendID string
	SessionID string
	PromptID  string
	// Prompt is the pending prompt, for approvals and questions.
	Prompt *PromptInfo
	// Account is the Claude account of a NoticeClaudeSignIn.
	Account   string
	CreatedAt time.Time
	SettledAt time.Time
}

// NoticeMessage is a Telegram message that shows a notice.
type NoticeMessage struct {
	ChatID    int64
	MessageID int64
}

// PushSubscription is one browser's Web Push subscription.
type PushSubscription struct {
	ID       string    `json:"id"`
	User     string    `json:"-"`
	Endpoint string    `json:"-"`
	P256dh   string    `json:"-"`
	Auth     string    `json:"-"`
	Label    string    `json:"label"`
	Created  time.Time `json:"created"`
}

// NotifyStore keeps notification settings, sent notices, Web Push
// subscriptions and where people are looking.
type NotifyStore interface {
	NotifySettings(ctx context.Context, user string) (NotifySettings, error)
	SetNotifySettings(ctx context.Context, user string, s NotifySettings) error
	// ClaimNotice stores r unless the person already has a notice with its
	// Key. claimed is false then, and r.ID is that notice's ID.
	ClaimNotice(ctx context.Context, r NoticeRecord) (id string, claimed bool, err error)
	Notice(ctx context.Context, id string) (NoticeRecord, bool, error)
	// SentSince counts a person's notices since t.
	SentSince(ctx context.Context, user string, t time.Time) (int, error)
	// SettleNotices marks the open notices of the prompts settled and
	// returns them.
	SettleNotices(ctx context.Context, backendID, sessionID string, promptIDs []string) ([]NoticeRecord, error)
	// DueReminders lists open approval and question notices created before
	// t that had no reminder yet and are not put off past now.
	DueReminders(ctx context.Context, t, now time.Time) ([]NoticeRecord, error)
	// DeferReminder puts a notice's reminder off until t.
	DeferReminder(ctx context.Context, id string, t time.Time) error
	// ClaimReminder marks a notice reminded; false when it already was.
	ClaimReminder(ctx context.Context, id string) (bool, error)
	AddNoticeMessage(ctx context.Context, id string, m NoticeMessage) error
	NoticeMessages(ctx context.Context, id string) ([]NoticeMessage, error)
	// PruneNotices drops notices and presence older than t.
	PruneNotices(ctx context.Context, t time.Time) error

	SavePushSubscription(ctx context.Context, s PushSubscription) (PushSubscription, error)
	PushSubscriptions(ctx context.Context, user string) ([]PushSubscription, error)
	// DeletePushSubscription deletes a person's subscription by ID or
	// endpoint; false when they have none such.
	DeletePushSubscription(ctx context.Context, user, idOrEndpoint string) (bool, error)

	// SetPresence records that user has the chat open in a visible page,
	// or, with visible false, that they left it.
	SetPresence(ctx context.Context, user, backendID, sessionID string, visible bool) error
	// Present reports whether user had the chat open in a visible page
	// since t.
	Present(ctx context.Context, user, backendID, sessionID string, t time.Time) (bool, error)

	// TelegramIDs lists the Telegram accounts linked to user.
	TelegramIDs(ctx context.Context, user string) ([]int64, error)
	// TelegramGroup is the forum group user linked, ok false for none.
	TelegramGroup(ctx context.Context, user string) (chatID int64, ok bool, err error)
	// TelegramAttached reports whether one of user's Telegram chats is
	// attached to the session, or the session has a topic in user's group.
	TelegramAttached(ctx context.Context, user, backendID, sessionID string) (bool, error)

	// CronMark is the last run the hub saw of a scheduled job; ok is
	// false for a job it never saw.
	CronMark(ctx context.Context, backendID, jobID string) (mark string, ok bool, err error)
	SetCronMark(ctx context.Context, backendID, jobID, mark string) error
}
