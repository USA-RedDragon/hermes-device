package hub

import (
	"context"
	"errors"
	"strings"
)

// BackgroundPrefix starts the ID of every /bg prompt's task and of the
// session Hermes stores it as. Hermes v0.23.1 marks such a session by its
// ID only: its row has the source of the surface that ran it and no parent
// session (tui_gateway/agent_callbacks.py _background_agent_kwargs).
const BackgroundPrefix = "bg_"

// IsBackground reports whether a session is the stored run of a /bg
// prompt. Such a session belongs to the chat that started it, so the hub
// leaves it out of the chat list, the search and Telegram topics.
func IsBackground(sessionID string) bool {
	return strings.HasPrefix(sessionID, BackgroundPrefix)
}

// Errors of Activity.
var (
	// ErrUnknownSubagent is a subagent the session does not run now.
	ErrUnknownSubagent = errors.New("unknown subagent")
	// ErrUnknownProcess is a background command the session does not own.
	ErrUnknownProcess = errors.New("unknown background command")
	// ErrSteerRejected is a message a subagent no longer takes: it is past
	// its last step or has ended.
	ErrSteerRejected = errors.New("the subagent no longer takes messages")
	// ErrNoToolBackground is a backend that cannot move a running command
	// to the background.
	ErrNoToolBackground = errors.New("this agent server cannot move a running command to the background; upgrade Talaria on the server")
	// ErrNoRunningTool is a turn with no running tool to move.
	ErrNoRunningTool = errors.New("no command of this chat is running")
	// ErrUsageUnavailable is token use the backend's records could not
	// give, wrapping why.
	ErrUsageUnavailable = errors.New("token use unavailable")
)

// Activity is the work a chat runs beside its turns: subagents that
// delegate_task started, background commands and /bg prompts. Every method
// fails with ErrForbidden when the backend is not mapped to user, and acts
// only on the work of the named session.
type Activity interface {
	// Activity lists the session's subagents and background commands.
	Activity(ctx context.Context, user, backendID, sessionID string) (ActivityState, error)
	// SubagentTail is the end of a running subagent's transcript.
	SubagentTail(ctx context.Context, user, backendID, sessionID, subagentID string) (SubagentTail, error)
	// SteerSubagent queues a message for a running subagent. It takes the
	// message at its next step.
	SteerSubagent(ctx context.Context, user, backendID, sessionID, subagentID, text string) error
	// StopSubagent interrupts a running subagent.
	StopSubagent(ctx context.Context, user, backendID, sessionID, subagentID string) error
	// StopProcess kills one background command of the session.
	StopProcess(ctx context.Context, user, backendID, sessionID, processID string) error
	// PauseSubagents stops or allows new subagents. Hermes keeps one switch
	// for every chat of the backend; running subagents go on.
	PauseSubagents(ctx context.Context, user, backendID, sessionID string, paused bool) (bool, error)
	// Background starts a /bg prompt: a separate agent runs text with the
	// session's settings, and its answer comes as a background.complete
	// event. It returns the task ID.
	Background(ctx context.Context, user, backendID, sessionID, text string) (string, error)
}

// ToolMover moves a running command of a turn to the background.
type ToolMover interface {
	// BackgroundTool moves the running terminal command toolCallID of the
	// session's turn to the background: the turn goes on and the command
	// runs on as a background command. With toolCallID "" every running
	// command moves. It fails with ErrNoToolBackground when the backend
	// cannot do this and ErrNoRunningTool when nothing runs.
	BackgroundTool(ctx context.Context, user, backendID, sessionID, toolCallID string) error
}

// ActivityState is what a session runs beside its turns now.
type ActivityState struct {
	Subagents []Subagent `json:"subagents"`
	Processes []Process  `json:"processes"`
	// Paused is true while Hermes starts no new subagents on the backend.
	Paused bool `json:"paused"`
}

// Subagent is one running subagent, from subagent.list. Finished
// subagents are known only from their subagent.complete events.
type Subagent struct {
	ID        string  `json:"id"`
	ParentID  string  `json:"parent_id,omitempty"`
	Depth     int     `json:"depth"`
	Goal      string  `json:"goal"`
	Model     string  `json:"model,omitempty"`
	StartedAt float64 `json:"started_at,omitempty"`
	Status    string  `json:"status"`
	ToolCount int     `json:"tool_count"`
	LastTool  string  `json:"last_tool,omitempty"`
	// LastPreview is what that tool was given, such as a terminal's command.
	LastPreview    string `json:"last_preview,omitempty"`
	AcceptingSteer bool   `json:"accepting_steer"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	// WaitingOn are the commands of the background processes a subagent with
	// status "waiting" ended its turn on; Hermes resumes it when one exits.
	WaitingOn []string `json:"waiting_on,omitempty"`
}

// Process is one background command of a session, running or ended.
type Process struct {
	ID        string  `json:"id"`
	Command   string  `json:"command"`
	Running   bool    `json:"running"`
	ExitCode  *int    `json:"exit_code"`
	StartedAt float64 `json:"started_at,omitempty"`
	ExitedAt  float64 `json:"exited_at,omitempty"`
	// Reason is why it ended, such as "exited" or "killed".
	Reason string `json:"reason,omitempty"`
	// Output is the end of its output: Hermes keeps the last 32k
	// characters.
	Output string `json:"output"`
}

// SubagentTail is the end of a subagent's live transcript.
type SubagentTail struct {
	Available bool   `json:"available"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// TurnTokens is what the hub adds to session.usage and message.complete
// payloads, under TurnKey: the tokens of the turn that runs or just ended.
// Input counts every token sent in, cached or not; it is nil when the hub
// cannot tell.
type TurnTokens struct {
	Input  *int64 `json:"input"`
	Output int64  `json:"output"`
}

// TurnKey is the payload member that carries TurnTokens.
const TurnKey = "hub_turn"
