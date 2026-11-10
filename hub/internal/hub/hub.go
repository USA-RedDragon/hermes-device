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
	Live                 bool
	UpdatedAt            time.Time
}

// Event kinds delivered on an attached session.
const (
	KindReasoningDelta  = "reasoning.delta"
	KindThinkingDelta   = "thinking.delta"
	KindToolStart       = "tool.start"
	KindToolProgress    = "tool.progress"
	KindToolComplete    = "tool.complete"
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
	Prompt(ctx context.Context, user, backendID, sessionID, text string) error
	Interrupt(ctx context.Context, user, backendID, sessionID string) error
	RespondApproval(ctx context.Context, user, backendID, sessionID, approvalID string, allow bool) error
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
