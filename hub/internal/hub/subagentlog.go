package hub

import (
	"context"
	"encoding/json"
	"time"
)

// Types of SubagentEvent.
const (
	// StepGoal is the task the subagent was given, its first user row.
	StepGoal = "goal"
	// StepUser is a later user row of the subagent's transcript, such as a
	// summary of earlier steps.
	StepUser = "user"
	// StepMessage is text the subagent wrote.
	StepMessage = "message"
	// StepThinking is the subagent's reasoning.
	StepThinking = "thinking"
	// StepToolStart is a tool call the subagent made.
	StepToolStart = "tool_start"
	// StepToolEnd is the result of a tool call.
	StepToolEnd = "tool_end"
	// StepSteer is a message the user sent the subagent through the hub.
	StepSteer = "steer"
	// StepCompaction says Hermes summarized earlier steps to make room.
	StepCompaction = "compaction"
)

// MaxStepResult bounds the result an event keeps, in characters.
const MaxStepResult = 8192

// SubagentEvent is one step of a subagent, as the hub keeps it. Seq orders
// the events of one subagent and is what a reader polls after; TS is when
// the step happened.
type SubagentEvent struct {
	Seq    int64     `json:"seq"`
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	CallID string    `json:"call_id,omitempty"`
	Name   string    `json:"name,omitempty"`
	// Args are the call's arguments, each string cut to MaxStepResult.
	Args json.RawMessage `json:"args,omitempty"`
	// Result is the start of the call's result, ResultSize its full
	// length in characters.
	Result     string `json:"result,omitempty"`
	ResultSize int    `json:"result_size,omitempty"`
	// DurationMS is how long the call ran, when Hermes recorded it.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	Error      bool   `json:"error,omitempty"`
	Text       string `json:"text,omitempty"`
}

// SubagentEvents is a page of a subagent's steps.
type SubagentEvents struct {
	Events []SubagentEvent `json:"events"`
	// Epoch changes when the hub read the transcript again from its start
	// (Hermes summarized or rewrote it): a reader then drops what it holds
	// and reads from seq 0.
	Epoch int64 `json:"epoch"`
	// Live says Hermes answered now; without it the events are the hub's
	// stored copy.
	Live bool `json:"live"`
	// Running says the subagent still runs.
	Running bool `json:"running"`
	// Details says the backend sends tool results and arguments.
	Details bool `json:"details"`
	// More says there are events after these; read again after the last.
	More bool `json:"more"`
}

// SubagentKey names one subagent of one user's chat.
type SubagentKey struct {
	User, BackendID, SessionID, SubagentID string
}

// SubagentTranscripts reads the steps of a chat's subagents, running or
// ended. It fails like Activity.
type SubagentTranscripts interface {
	SubagentEvents(ctx context.Context, user, backendID, sessionID, subagentID string, after int64) (SubagentEvents, error)
}

// SubagentLog keeps the steps of subagents, so they stay readable after a
// subagent ends and each step keeps its seq. Events of one key are only
// ever served for that key.
type SubagentLog interface {
	// SubagentCursor is where the hub's last read of the subagent's
	// transcript ended, and the log's epoch.
	SubagentCursor(ctx context.Context, k SubagentKey) (cursor string, epoch int64, err error)
	// AppendSubagentEvents adds evs, numbering them after the key's
	// events, and moves the cursor from the value from to the value to. With
	// reset it first
	// drops the events read from the transcript (steers stay) and starts a
	// new epoch. It stores nothing and returns false when the cursor is no
	// longer from: another read got there first.
	AppendSubagentEvents(ctx context.Context, k SubagentKey, from, to string, reset bool, evs []SubagentEvent) (bool, error)
	// SubagentEvents lists up to limit events with a seq above after.
	SubagentEvents(ctx context.Context, k SubagentKey, after int64, limit int) ([]SubagentEvent, error)
	// AddSubagentSteer stores a message the user sent the subagent.
	AddSubagentSteer(ctx context.Context, k SubagentKey, text string, at time.Time) (SubagentEvent, error)
	// PruneSubagentLogs drops the logs not written since t.
	PruneSubagentLogs(ctx context.Context, t time.Time) error
}
