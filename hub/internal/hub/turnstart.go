package hub

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// Fields the hub adds to a message.start payload. StartSubmissionID and
// StartAdmissionID are the submission and admission IDs of the input that
// started the turn, when the hub knows them; StartedAt is when the hub saw
// the turn start, in Unix milliseconds.
const (
	StartSubmissionID = "submission_id"
	StartAdmissionID  = "admission_id"
	StartedAt         = "started_at"
)

// submissionPrefix starts the submission IDs of the prompts that the hub's
// web UI and API send, so every front end can tell them from those of
// other clients.
const submissionPrefix = "hub:"

// NewSubmissionID is a new submission ID for a prompt the hub sends.
func NewSubmissionID() string {
	return submissionPrefix + uuid.NewString()
}

// FromHub reports whether a submission ID is one of NewSubmissionID's.
func FromHub(id string) bool {
	return strings.HasPrefix(id, submissionPrefix)
}

// OpenPrompt is the user message that started the turn a transcript ends
// in.
type OpenPrompt struct {
	Text string
	// SubmissionID is the submission ID Hermes stored on the row, "" when
	// it stored none.
	SubmissionID string
	// Images counts the images the row carries as content parts.
	Images int
}

// noteKind reports whether a user row's display kind is one that Hermes
// writes for a notice, not for what the user typed.
func noteKind(kind string) bool {
	switch kind {
	case "internal_notification", "process_complete", "async_delegation_complete":
		return true
	}
	return false
}

type transcriptRow struct {
	Role         string          `json:"role"`
	Text         *string         `json:"text"`
	Content      json.RawMessage `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	DisplayKind  string          `json:"display_kind"`
	SubmissionID string          `json:"submission_id"`
}

// OpenTurnPrompt finds the user message of the turn a transcript ends in:
// the last user row that no reply follows, only tool rows and assistant
// rows that call tools. It is false when the transcript ends with a reply,
// so a turn whose prompt Hermes has not stored yet never shows the prompt
// of the turn before.
func OpenTurnPrompt(rows []json.RawMessage) (OpenPrompt, bool) {
	for i := len(rows) - 1; i >= 0; i-- {
		var r transcriptRow
		if json.Unmarshal(rows[i], &r) != nil {
			return OpenPrompt{}, false
		}
		switch r.Role {
		case "tool":
			continue
		case "assistant":
			if calls := strings.TrimSpace(string(r.ToolCalls)); calls != "" && calls != "null" && calls != "[]" {
				continue
			}
			return OpenPrompt{}, false
		case "user":
			if r.DisplayKind == "hidden" || noteKind(r.DisplayKind) {
				return OpenPrompt{}, false
			}
			p := OpenPrompt{SubmissionID: r.SubmissionID}
			if r.Text != nil {
				p.Text = *r.Text
			}
			text, images := rowContent(r.Content)
			if r.Text == nil {
				p.Text = text
			}
			p.Images = images
			return p, p.Text != "" || p.Images > 0
		default:
			return OpenPrompt{}, false
		}
	}
	return OpenPrompt{}, false
}

// rowContent reads a row's content: a string, or content parts with text
// and images.
func rowContent(raw json.RawMessage) (string, int) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", 0
	}
	var texts []string
	images := 0
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			texts = append(texts, p.Text)
		case "image_url", "input_image", "image":
			images++
		}
	}
	return strings.Join(texts, "\n"), images
}
