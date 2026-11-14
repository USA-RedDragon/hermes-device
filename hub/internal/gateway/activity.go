package gateway

// Methods of a session's subagents and background work, as the shared
// owner serves them (gateway/session_ancillary.py, session_tui_side.py and
// session_tui_ops.py). Each one acts only on the named session's own
// children and processes.
const (
	MethodSubagentList      = "subagent.list"
	MethodSubagentTail      = "subagent.tail"
	MethodSubagentSteer     = "subagent.steer"
	MethodSubagentInterrupt = "subagent.interrupt"
	MethodSubagentRoster    = "subagent.roster"
	MethodProcessList       = "process.list"
	MethodProcessKill       = "process.kill"
	MethodDelegationStatus  = "delegation.status"
	MethodDelegationPause   = "delegation.pause"
	MethodPromptBackground  = "prompt.background"
)

// BackgroundCompleteEvent ends a /bg prompt: {"task_id", "text"}.
const BackgroundCompleteEvent = "background.complete"

// SessionParams names one session and nothing else, as process.list and
// subagent.list take it.
type SessionParams struct {
	SessionID string `json:"session_id"`
}

// SubagentParams names one subagent of a session (subagent.tail,
// subagent.interrupt). The owner refuses any other key.
type SubagentParams struct {
	SessionID  string `json:"session_id"`
	SubagentID string `json:"subagent_id"`
}

// SteerParams is subagent.steer's params.
type SteerParams struct {
	SessionID  string `json:"session_id"`
	SubagentID string `json:"subagent_id"`
	Text       string `json:"text"`
}

// SubagentRow is one subagent.list row (session_ancillary.subagent_row).
type SubagentRow struct {
	SubagentID     string   `json:"subagent_id"`
	ParentID       *string  `json:"parent_id"`
	Depth          *int     `json:"depth"`
	Goal           string   `json:"goal"`
	DelegationID   *string  `json:"delegation_id"`
	Model          *string  `json:"model"`
	StartedAt      *float64 `json:"started_at"`
	Status         *string  `json:"status"`
	ToolCount      *int     `json:"tool_count"`
	LastTool       *string  `json:"last_tool"`
	LastPreview    *string  `json:"last_preview"`
	AcceptingSteer *bool    `json:"accepting_steer"`
	ChildSessionID *string  `json:"child_session_id"`
}

// SubagentList is subagent.list's result.
type SubagentList struct {
	Subagents []SubagentRow `json:"subagents"`
}

// SubagentTail is subagent.tail's result: the end of the child's live
// transcript, at most 16 KiB.
type SubagentTail struct {
	SubagentID string `json:"subagent_id"`
	Available  bool   `json:"available"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
}

// SteerResult is subagent.steer's result: "queued" or "rejected".
type SteerResult struct {
	Status string `json:"status"`
}

// InterruptResult is subagent.interrupt's result.
type InterruptResult struct {
	Found bool `json:"found"`
}

// ProcessRow is one process.list row (session_ancillary.process_snapshot).
// SessionID is the process's id in the registry.
type ProcessRow struct {
	SessionID        string   `json:"session_id"`
	Command          string   `json:"command"`
	Status           string   `json:"status"`
	ExitCode         *int     `json:"exit_code"`
	StartedEpoch     *float64 `json:"started_epoch"`
	ExitedAt         *float64 `json:"exited_at"`
	CompletionReason *string  `json:"completion_reason"`
	OutputTail       string   `json:"output_tail"`
}

// ProcessList is process.list's result.
type ProcessList struct {
	Processes []ProcessRow `json:"processes"`
}

// ProcessKillParams is process.kill's params.
type ProcessKillParams struct {
	SessionID string `json:"session_id"`
	ProcessID string `json:"process_id"`
}

// ProcessKillResult is the part of process.kill's result the hub reads
// (tools/process_registry.py kill_process).
type ProcessKillResult struct {
	Status string `json:"status"`
}

// DelegationStatus is the part of delegation.status's result the hub
// passes on. The result's "active" lists the subagents of every chat of
// the gateway, so the hub never passes it on.
type DelegationStatus struct {
	Paused bool `json:"paused"`
}

// DelegationPauseParams is delegation.pause's params. With a session the
// owner checks that the caller may control that session; the gate itself
// covers every chat of the gateway process.
type DelegationPauseParams struct {
	SessionID string `json:"session_id"`
	Paused    bool   `json:"paused"`
}

// BackgroundParams is prompt.background's params.
type BackgroundParams struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// BackgroundResult is prompt.background's result. The answer comes later
// as a background.complete event with the same task_id.
type BackgroundResult struct {
	TaskID string `json:"task_id"`
}

// Roster is subagent.roster's result: the stored sessions of the profile
// that have subagents or background processes running now
// (tui_gateway/methods_subagents.py).
type Roster struct {
	Sessions []struct {
		SessionID  string `json:"session_id"`
		Subagents  int    `json:"subagents"`
		Background int    `json:"background"`
	} `json:"sessions"`
}
