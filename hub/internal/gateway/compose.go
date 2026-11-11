package gateway

import "encoding/json"

// Methods for steering, rewinding, branching, reading and running slash
// commands (gateway/session_queued_controls.py, gateway/session_mutations.py,
// gateway/session_stored_reads.py, gateway/session_discovery.py and
// gateway/session_commands.py).
const (
	MethodSteerQueued     = "prompt.steer_queued"
	MethodTranscript      = "session.transcript"
	MethodCommandsCatalog = "commands.catalog"
	MethodSlashExec       = "slash.exec"
	MethodSessionInfo     = "session.info"
)

// session.mutate operations for history (hermes_state_mutations.py).
const (
	OpRewind = "rewind"
	OpBranch = "branch"
)

// Refusals of prompt.steer_queued. Each leaves the admission queued, or
// started as its own turn: none loses the text.
const (
	ReasonAdmissionStarted   = "admission_started"
	ReasonSessionIdle        = "session_idle"
	ReasonSteerUnavailable   = "steer_unavailable"
	ReasonUnsupportedControl = "unsupported_control"
	// ReasonUnsupportedCommand is slash.exec's refusal of a command it does
	// not run for a local session; its data may carry a hint in "detail".
	ReasonUnsupportedCommand = "unsupported_command"
)

// Receipt is the admission receipt prompt.submit and prompt.steer_queued
// answer (tui_gateway/contracts/canonical.py AdmissionStatus). Status is
// "queued", "started", "unknown" or "terminal".
type Receipt struct {
	AdmissionID         string `json:"admission_id"`
	Status              string `json:"status"`
	Outcome             string `json:"outcome"`
	ExecutionGeneration *int64 `json:"execution_generation"`
}

// SteerQueuedParams is prompt.steer_queued's params.
type SteerQueuedParams struct {
	SessionID   string `json:"session_id"`
	AdmissionID string `json:"admission_id"`
}

// RewindPayload is session.mutate's payload for the rewind operation: it
// drops the user row TargetMessageID and every row after it.
type RewindPayload struct {
	TargetMessageID int64 `json:"target_message_id"`
}

// RewindResult is the part of a rewind's mutate result the hub reads.
// TargetMessage is the dropped user row, its content the typed text.
type RewindResult struct {
	MutateResult
	RewoundCount  int `json:"rewound_count"`
	TargetMessage struct {
		Content json.RawMessage `json:"content"`
	} `json:"target_message"`
}

// BranchPayload is session.mutate's payload for the branch operation.
// ThroughMessageID is the row id of the last row the branch keeps; 0 keeps
// the whole transcript.
type BranchPayload struct {
	Title            string `json:"title,omitempty"`
	ThroughMessageID int64  `json:"through_message_id,omitempty"`
}

// BranchResult is the part of a branch's mutate result the hub reads.
type BranchResult struct {
	MutateResult
	BranchedSessionID string `json:"branched_session_id"`
}

// Transcript orders.
const (
	OrderOldest = "oldest"
	OrderLatest = "latest"
)

// TranscriptParams is session.transcript's params. Limit is at most
// MaxTranscriptPage.
type TranscriptParams struct {
	SessionID string `json:"session_id"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Order     string `json:"order,omitempty"`
}

// MaxTranscriptPage is the most rows one session.transcript page has
// (gateway/session_stored_reads.py MAX_PAGE_ROWS).
const MaxTranscriptPage = 500

// TranscriptResult is session.transcript's result. Rows are oldest first in
// either order; a row is a display row with "row_kind" and "row_id". Reset
// says the rows start the transcript again.
type TranscriptResult struct {
	Rows       []json.RawMessage `json:"rows"`
	NextCursor *string           `json:"next_cursor"`
	Reset      bool              `json:"reset"`
}

// CatalogCategory is one category of commands.catalog: rows of
// [name, description].
type CatalogCategory struct {
	Name  string      `json:"name"`
	Pairs [][2]string `json:"pairs"`
}

// CatalogResult is the part of commands.catalog's result the hub reads.
type CatalogResult struct {
	Categories []CatalogCategory `json:"categories"`
	// Skills are the skill commands by "/name".
	Skills map[string]json.RawMessage `json:"skills"`
	Pairs  [][2]string                `json:"pairs"`
}

// SlashExecParams is slash.exec's params: Command is the command line
// without its leading slash.
type SlashExecParams struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
}

// slash.exec result types.
const (
	SlashOutput = "exec"
	SlashSend   = "send"
	SlashSkill  = "skill"
)

// SlashExecResult is slash.exec's result: Output for "exec", or a Message
// the client submits as the next prompt for "send" and "skill".
type SlashExecResult struct {
	Type    string `json:"type"`
	Output  string `json:"output"`
	Message string `json:"message"`
	Notice  string `json:"notice"`
}
