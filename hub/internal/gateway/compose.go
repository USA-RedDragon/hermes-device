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
	// OpCompress summarizes older messages (hermes_state_mutation_compress.py).
	OpCompress = "compress"
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
	// SubagentID reads a delegated child of the session instead.
	SubagentID string `json:"subagent_id,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Order      string `json:"order,omitempty"`
	// ToolDetails asks for tool calls and results, up to that many chars
	// each (Talaria Hermes patch 0051; only when runtime.describe lists
	// CapTranscriptToolDetails).
	ToolDetails int `json:"tool_details,omitempty"`
}

// CapTranscriptToolDetails is the runtime.describe capability of a Hermes
// whose session.transcript takes tool_details.
const CapTranscriptToolDetails = "transcript-tool-details-v1"

// MaxToolDetails is the most tool_details takes.
const MaxToolDetails = 65536

// ChildTranscript is session.transcript's result with subagent_id: a page
// of the child's rows, and whether Hermes found the child and whether it
// still runs.
type ChildTranscript struct {
	TranscriptResult
	Available bool `json:"available"`
	Running   bool `json:"running"`
}

// TranscriptRow is the part of a session.transcript row the hub reads for a
// subagent's steps. ToolCalls, Result and ResultSize come only with
// tool_details.
type TranscriptRow struct {
	RowKind   string   `json:"row_kind"`
	Role      string   `json:"role"`
	Text      string   `json:"text"`
	Timestamp *float64 `json:"timestamp"`
	RowID     *int64   `json:"row_id"`
	// Reasoning and ReasoningContent are the assistant's reasoning
	// sidecars, text when Hermes has it as text.
	Reasoning        json.RawMessage `json:"reasoning"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	ToolCalls        []struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
	Result     *string         `json:"result"`
	ResultSize int             `json:"result_size"`
	// DisplayMetadata carries tool_outcome (ToolOutcome) on a tool row when
	// sessions.tool_outcome_metadata is on.
	DisplayMetadata json.RawMessage `json:"display_metadata"`
}

// ToolOutcome is how long a stored tool call ran and whether it failed.
type ToolOutcome struct {
	DurationMS int64 `json:"duration_ms"`
	IsError    bool  `json:"is_error"`
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
	// Canon maps each "/name" and alias, in lower case, to the command's
	// own "/name".
	Canon map[string]string `json:"canon"`
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

// CompressPayload is session.mutate's payload for the compress operation:
// Focus is the raw /compress argument ("here 3", "--preview", a topic).
type CompressPayload struct {
	Focus string `json:"focus,omitempty"`
}

// CompressResult is the part of a compress's mutate result the hub reads.
// A preview carries Lines; a compress that ran or removed nothing carries
// Summary (agent/manual_compression_feedback.py).
type CompressResult struct {
	MutateResult
	Lines   []string `json:"lines"`
	Summary *struct {
		Headline  string `json:"headline"`
		TokenLine string `json:"token_line"`
		Note      string `json:"note"`
	} `json:"summary"`
}
