// Package gateway is a client for the Hermes Agent gateway's canonical
// session WebSocket (/api/ws, JSON-RPC 2.0, one JSON object per message),
// pinned to Hermes v0.23.1.
package gateway

import (
	"encoding/json"
	"strings"
)

// HermesVersion is the Hermes Agent release this client was checked against.
const HermesVersion = "v0.23.1"

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
	MethodPromptCancel  = "prompt.cancel"
	MethodResolveLost   = "prompt.resolve_unknown"
	MethodSessionDetach = "session.detach"
	MethodEventsSince   = "session.events.since"
	MethodPromptSubmit  = "prompt.submit"
	MethodInterrupt     = "session.interrupt"
	MethodSteer         = "session.steer"
	MethodApproval      = "approval.respond"
	MethodClarify       = "clarify.respond"
	MethodConfigSet     = "config.set"
	MethodConfigGet     = "config.get"
	MethodMutate        = "session.mutate"
	MethodModelOptions  = "model.options"
	MethodAttachBytes   = "image.attach_bytes"
	MethodFileAttach    = "file.attach"
)

// session.mutate operations (hermes_state_mutations.py validate_action).
const (
	OpRename  = "rename"
	OpArchive = "archive"
	OpDelete  = "delete"
	OpModel   = "model"
)

// Refusal reasons the hub acts on. A refusal is a JSON-RPC error whose
// message is the reason (gateway/session_controls.py _refusal_data).
const (
	ReasonRevisionConflict = "revision_conflict"
	ReasonStaleGeneration  = "stale_generation"
	ReasonSessionBusy      = "session_busy"
	ReasonUnknownExecution = "unknown_execution"
	ReasonInvalidParams    = "invalid_params"
	ReasonNotFound         = "not_found"
	ReasonModelResolution  = "model_resolution_failed"
	// ReasonRuntimeDraining is the retryable refusal of a gateway that is
	// starting or retiring (gateway/session_authorities.py).
	ReasonRuntimeDraining = "runtime_draining"
)

// Session events the hub reads besides the turn events.
const (
	UpdatedEvent = "session.updated"
	UsageEvent   = "session.usage"
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
	AdmissionID         string          `json:"admission_id,omitempty"`
}

// Session sources (CanonicalCreateParams.source). A session.list row
// carries the source of the surface that started the session.
const (
	SourceGUI = "gui"
	SourceCLI = "cli"
	SourceTUI = "tui"
	SourceACP = "acp"
)

// session.list scopes.
const (
	ScopeLive   = "live"
	ScopeStored = "stored"
)

// ListParams is session.list's params. IncludeHidden also lists archived
// and hidden sessions, with nothing in the row to tell them apart.
type ListParams struct {
	Scope         string `json:"scope"`
	Limit         int    `json:"limit"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
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
// RowCWD is the cwd recorded on the session's row when it differs from
// CWD, where the tools run.
type CreateParams struct {
	RequestID string `json:"request_id"`
	Source    string `json:"source"`
	CWD       string `json:"cwd,omitempty"`
	RowCWD    string `json:"row_cwd,omitempty"`
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
	MultiSelect         bool     `json:"multi_select,omitempty"`
}

// Prompt kinds.
const (
	PromptApproval = "approval"
	PromptClarify  = "clarify"
)

// SnapshotInfo is the part of a snapshot's info, and of a session.info
// payload, the hub reads (gateway/session_local.py local_session_info and
// gateway/session_local_inventory.py snapshot_info). Usage has the shape of
// message.complete's usage (gateway/session_usage.py tui_usage).
type SnapshotInfo struct {
	Title           *string         `json:"title,omitempty"`
	Model           *string         `json:"model,omitempty"`
	Provider        *string         `json:"provider,omitempty"`
	CWD             *string         `json:"cwd,omitempty"`
	ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
	ContextLength   *int64          `json:"context_length,omitempty"`
	Usage           json.RawMessage `json:"usage,omitempty"`
	// Vision says whether the session can read images (see Vision).
	Vision *bool `json:"-"`
}

// UnmarshalJSON reads each field on its own and skips one of an unexpected
// type, so a change in a field the hub only displays cannot stop a session
// from opening.
func (i *SnapshotInfo) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	str := func(k string) *string {
		var s string
		if string(m[k]) == "null" || json.Unmarshal(m[k], &s) != nil {
			return nil
		}
		return &s
	}
	*i = SnapshotInfo{Title: str("title"), Model: str("model"), Provider: str("provider"), CWD: str("cwd"),
		ReasoningEffort: str("reasoning_effort"), Vision: Vision(b)}
	var n float64
	if json.Unmarshal(m["context_length"], &n) == nil {
		v := int64(n)
		i.ContextLength = &v
	}
	if u := m["usage"]; len(u) > 0 && u[0] == '{' {
		i.Usage = u
	}
	return nil
}

// Admission statuses in a snapshot's pending list.
const (
	AdmissionQueued  = "queued"
	AdmissionUnknown = "unknown"
)

// PendingAdmission is one input in a snapshot's pending list: queued behind
// the running turn, or "unknown" when its turn was lost in a gateway restart.
// An unknown row holds back every input queued after it until it is resolved.
type PendingAdmission struct {
	AdmissionID         string `json:"admission_id"`
	Sequence            int64  `json:"sequence"`
	Status              string `json:"status"`
	ExecutionGeneration *int64 `json:"execution_generation"`
	Text                string `json:"text"`
	InputID             string `json:"input_id,omitempty"`
}

// Producer is the producer that queued an input when Hermes queued it
// itself, such as "cron" for a cron result, else "". Hermes names such an
// input by a JSON array whose first element is the producer
// (gateway/session_automation.py admit_route_wake); a user's input has the
// submission ID of the client.
func Producer(inputID string) string {
	if !strings.HasPrefix(inputID, "[") {
		return ""
	}
	var parts []any
	if json.Unmarshal([]byte(inputID), &parts) != nil || len(parts) == 0 {
		return ""
	}
	p, _ := parts[0].(string)
	return p
}

// InfoEvent is the session.info event type. Its payload carries the pending
// list; message.start names the admission it started but not its text.
const InfoEvent = "session.info"

// InfoPayload is the part of a session.info payload the hub reads. The
// pending list and revision come in their own session.info frames
// (gateway/session_authority.py _publish_pending); the session's settings
// come in others.
type InfoPayload struct {
	Pending  []PendingAdmission `json:"pending"`
	Revision *int64             `json:"revision"`
}

// MutateParams is session.mutate's params. ExpectedGeneration is required
// for delete, model and reasoning, and refused as null by none.
type MutateParams struct {
	SessionID          string `json:"session_id"`
	RequestID          string `json:"request_id"`
	ExpectedRevision   int64  `json:"expected_revision"`
	Operation          string `json:"operation"`
	Payload            any    `json:"payload"`
	ExpectedGeneration *int64 `json:"expected_generation,omitempty"`
}

// MutateResult is the part of a session.mutate result the hub reads.
// Status is set only by a model pick that did not commit: "deferred" while
// a turn runs, "confirmation_required", or "noop".
type MutateResult struct {
	Revision            *int64 `json:"revision"`
	ExecutionGeneration *int64 `json:"execution_generation"`
	Status              string `json:"status"`
	Confirm             string `json:"confirm"`
	ConfirmMessage      string `json:"confirm_message"`
}

// Fences is the fresh revision and generation a revision_conflict or
// stale_generation refusal carries in its data.
type Fences struct {
	Revision            *int64 `json:"revision"`
	ExecutionGeneration *int64 `json:"execution_generation"`
}

// ModelPayload is session.mutate's payload for the model operation.
type ModelPayload struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
	Confirm  string `json:"confirm,omitempty"`
}

// ModelOptionsParams is model.options's params.
type ModelOptionsParams struct {
	SessionID string `json:"session_id"`
}

// ModelCapabilities is one model's capabilities in a model.options row.
type ModelCapabilities struct {
	Reasoning           bool  `json:"reasoning"`
	CanDisableReasoning *bool `json:"can_disable_reasoning"`
}

// ModelProvider is one provider row of model.options.
type ModelProvider struct {
	Slug              string                       `json:"slug"`
	Name              string                       `json:"name"`
	Models            []string                     `json:"models"`
	Authenticated     *bool                        `json:"authenticated"`
	UnavailableModels []string                     `json:"unavailable_models"`
	Capabilities      map[string]ModelCapabilities `json:"capabilities"`
}

// ModelOptionsResult is model.options's result.
type ModelOptionsResult struct {
	Providers []ModelProvider `json:"providers"`
	Model     string          `json:"model"`
	Provider  string          `json:"provider"`
}

// ConfigGetParams is config.get's params.
type ConfigGetParams struct {
	SessionID string `json:"session_id"`
	Key       string `json:"key"`
}

// KeyYolo is the config.get key of a session's approval bypass
// (gateway/session_busy_controls.py yolo_config): "1" when its commands
// run without approvals.
const KeyYolo = "yolo"

// ReasoningKey is the config.set key for a session's reasoning effort
// (gateway/session_tui_config.py _set_command_setting). It runs the fenced
// mutation itself and holds the change while a turn runs.
const ReasoningKey = "reasoning"

// ReasoningReset is the reasoning value that returns a session to the
// profile's default effort.
const ReasoningReset = "reset"

// AdmissionParams is prompt.cancel's params.
type AdmissionParams struct {
	SessionID   string `json:"session_id"`
	AdmissionID string `json:"admission_id"`
}

// ResolveLostParams is prompt.resolve_unknown's params.
type ResolveLostParams struct {
	SessionID           string `json:"session_id"`
	AdmissionID         string `json:"admission_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
}

// Snapshot is the result of session.resume and session.create.
type Snapshot struct {
	SessionID           string             `json:"session_id"`
	Messages            []json.RawMessage  `json:"messages"`
	Running             bool               `json:"running"`
	ReplayEpoch         string             `json:"replay_epoch"`
	LastSequence        int64              `json:"last_sequence"`
	SubscriptionID      string             `json:"subscription_id"`
	ExecutionGeneration int64              `json:"execution_generation"`
	Prompts             []Prompt           `json:"prompts"`
	Pending             []PendingAdmission `json:"pending"`
	Info                SnapshotInfo       `json:"info"`
	Revision            int64              `json:"revision"`
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
// Hermes keys an admission by principal, session and SubmissionID
// (hermes_state_runtime.py admit_session_input): a retry under the same
// SubmissionID and text answers the admission it already holds, also
// after a gateway restart, so the hub can resend a prompt whose answer it
// lost without running it twice.
type SubmitParams struct {
	SessionID    string       `json:"session_id"`
	Text         string       `json:"text"`
	SubmissionID string       `json:"submission_id"`
	Attachments  []Attachment `json:"attachments,omitempty"`
}

// Attachment is one image a prompt.submit carries: a path that
// image.attach_bytes staged in the profile's cache/images, and its type
// (gateway/session_ingress_media.py admit_attachments).
type Attachment struct {
	Path string `json:"path"`
	MIME string `json:"mime"`
}

// AttachBytesParams is image.attach_bytes's params. Filename only hints
// the extension; the gateway names the staged file itself
// (gateway/session_images.py).
type AttachBytesParams struct {
	SessionID     string `json:"session_id"`
	ContentBase64 string `json:"content_base64"`
	Filename      string `json:"filename,omitempty"`
}

// AttachBytesResult is the part of image.attach_bytes's result the hub
// reads.
type AttachBytesResult struct {
	Path string `json:"path"`
	MIME string `json:"mime"`
}

// FileAttachParams is file.attach's params for bytes the gateway cannot
// read from a path: it stages them in the profile's attachments folder
// (gateway/session_ancillary.py attach_file).
type FileAttachParams struct {
	SessionID string `json:"session_id"`
	DataURL   string `json:"data_url"`
	Name      string `json:"name"`
}

// FileAttachResult is the part of file.attach's result the hub reads.
type FileAttachResult struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// VisionTool is the tool that loads an image for the model. Hermes
// registers it only when the main model can see images or an auxiliary
// vision model is set (tools/vision_tools.py check_vision_requirements),
// so a session whose tools lack it cannot read attached images.
const VisionTool = "vision_analyze"

// Vision reads a session.info or snapshot info payload's tools, grouped by
// toolset (tui_gateway/server.py _session_info). It is nil when the
// payload lists no tools, as for a session whose agent is not loaded yet.
func Vision(info json.RawMessage) *bool {
	var p struct {
		Tools map[string][]string `json:"tools"`
	}
	if json.Unmarshal(info, &p) != nil || len(p.Tools) == 0 {
		return nil
	}
	found := false
	for _, names := range p.Tools {
		for _, n := range names {
			found = found || n == VisionTool
		}
	}
	return &found
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

// ClarifyParams is clarify.respond's params. An empty Answer skips the
// question; a multi-select answer is a JSON array of choice labels
// (tools/clarify_tool.py _clean_answer).
type ClarifyParams struct {
	SessionID           string `json:"session_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
	PromptID            string `json:"prompt_id"`
	Answer              string `json:"answer"`
}

// ConfigSetParams is config.set's params for a session control.
type ConfigSetParams struct {
	SessionID string `json:"session_id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}
