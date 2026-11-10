// Package gateway is a client for the Hermes Agent gateway's canonical
// session WebSocket (/api/ws, JSON-RPC 2.0, one JSON object per message),
// pinned to Hermes v0.22.7.
package gateway

import "encoding/json"

// HermesVersion is the Hermes Agent release this client was checked against.
const HermesVersion = "v0.22.7"

// Wire names from gateway/session_contract.py and hermes_cli/gateway_client.py.
const (
	Subprotocol    = "hermes-gateway-v1"
	TicketPrefix   = "hermes-gateway-ticket."
	ReadyEvent     = "gateway.ready"
	ReplayGapEvent = "session.replay_gap"
)

// Methods of the canonical session contract
// (tui_gateway/contracts/canonical_sessions.py).
const (
	MethodSessionList   = "session.list"
	MethodSessionCreate = "session.create"
	MethodSessionResume = "session.resume"
	MethodSessionDetach = "session.detach"
	MethodEventsSince   = "session.events.since"
	MethodPromptSubmit  = "prompt.submit"
	MethodInterrupt     = "session.interrupt"
	MethodSteer         = "session.steer"
	MethodApproval      = "approval.respond"
	MethodConfigSet     = "config.set"
)

// Approval choices (gateway/session_pending_controls.py).
const (
	ChoiceOnce = "once"
	ChoiceDeny = "deny"
)

// Event is the params of one "event" notification
// (gateway/session_events.py SessionEvents.publish).
type Event struct {
	Type                string          `json:"type"`
	SessionID           string          `json:"session_id"`
	Payload             json.RawMessage `json:"payload"`
	ReplayEpoch         string          `json:"replay_epoch"`
	Seq                 int64           `json:"seq"`
	ExecutionGeneration *int64          `json:"execution_generation,omitempty"`
}

// ListParams is session.list's params.
type ListParams struct {
	Scope string `json:"scope"`
	Limit int    `json:"limit"`
}

// ListRow is one session.list row.
type ListRow struct {
	SessionID    string   `json:"session_id"`
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Source       *string  `json:"source"`
	StartedAt    *float64 `json:"started_at"`
	MessageCount int      `json:"message_count"`
	Running      bool     `json:"running"`
}

// ListResult is session.list's result.
type ListResult struct {
	Sessions []ListRow `json:"sessions"`
	Scope    string    `json:"scope"`
}

// CreateParams is the subset of session.create's params the hub sends.
type CreateParams struct {
	RequestID string `json:"request_id"`
	Source    string `json:"source"`
	CWD       string `json:"cwd,omitempty"`
}

// ResumeParams is session.resume's params.
type ResumeParams struct {
	SessionID string `json:"session_id"`
}

// Prompt is one pending approval or clarify prompt.
type Prompt struct {
	Kind                string   `json:"kind"`
	PromptID            string   `json:"prompt_id"`
	ExecutionGeneration int64    `json:"execution_generation"`
	Choices             []string `json:"choices"`
	Command             *string  `json:"command,omitempty"`
	Description         *string  `json:"description,omitempty"`
	Question            *string  `json:"question,omitempty"`
}

// SnapshotInfo is the part of a snapshot's info the hub reads.
type SnapshotInfo struct {
	Title *string `json:"title,omitempty"`
	Model *string `json:"model,omitempty"`
}

// Snapshot is the result of session.resume and session.create.
type Snapshot struct {
	SessionID           string            `json:"session_id"`
	Messages            []json.RawMessage `json:"messages"`
	Running             bool              `json:"running"`
	ReplayEpoch         string            `json:"replay_epoch"`
	LastSequence        int64             `json:"last_sequence"`
	SubscriptionID      string            `json:"subscription_id"`
	ExecutionGeneration int64             `json:"execution_generation"`
	Prompts             []Prompt          `json:"prompts"`
	Info                SnapshotInfo      `json:"info"`
}

// DetachParams is session.detach's params.
type DetachParams struct {
	SessionID      string `json:"session_id"`
	SubscriptionID string `json:"subscription_id"`
}

// EventsSinceParams is session.events.since's params.
type EventsSinceParams struct {
	SessionID    string `json:"session_id"`
	LastSequence int64  `json:"last_sequence"`
	ReplayEpoch  string `json:"replay_epoch,omitempty"`
}

// EventsSinceResult is session.events.since's result.
type EventsSinceResult struct {
	Events           []Event `json:"events"`
	LastSequence     int64   `json:"last_sequence"`
	ReplayEpoch      string  `json:"replay_epoch"`
	SnapshotRequired bool    `json:"snapshot_required"`
}

// SubmitParams is the subset of prompt.submit's params the hub sends.
type SubmitParams struct {
	SessionID    string `json:"session_id"`
	Text         string `json:"text"`
	SubmissionID string `json:"submission_id"`
}

// InterruptParams is session.interrupt's params.
type InterruptParams struct {
	SessionID           string `json:"session_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
}

// ApprovalParams is approval.respond's params. The gateway refuses any
// other key.
type ApprovalParams struct {
	SessionID           string `json:"session_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
	PromptID            string `json:"prompt_id"`
	Choice              string `json:"choice"`
}

// ConfigSetParams is config.set's params for a session control.
type ConfigSetParams struct {
	SessionID string `json:"session_id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}
