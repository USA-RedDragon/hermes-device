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

// SubmissionIDFor is the submission ID of a prompt the hub sends for seed:
// the same for the same seed, so a retry never runs it twice.
func SubmissionIDFor(seed string) string {
	return submissionPrefix + uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

// MaxClientID bounds the length of a page's message id.
const MaxClientID = 64

// ValidClientID reports whether id is a message id a page may send: the
// hub prefix and a tail of at least 8 letters, digits and dashes. The page
// makes it up, and sending the same id again is a retry of one message.
func ValidClientID(id string) bool {
	tail, ok := strings.CutPrefix(id, submissionPrefix)
	if !ok || len(id) > MaxClientID || len(tail) < 8 {
		return false
	}
	for _, c := range tail {
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
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
	RowID        int64           `json:"row_id"`
	RowKind      string          `json:"row_kind"`
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
			if _, ok := CompactionSummary(p.Text); ok {
				return OpenPrompt{}, false
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

// UserMessage is a message the user sent, as a transcript row shows it.
type UserMessage struct {
	// RowID is the row's id in Hermes' store, which a rewind or a branch
	// names.
	RowID  int64
	Text   string
	Images int
}

// LastUserMessage finds the last row of rows (oldest first) that the user
// typed: a user row with a row id that is not hidden and not a note Hermes
// wrote.
func LastUserMessage(rows []json.RawMessage) (UserMessage, bool) {
	for i := len(rows) - 1; i >= 0; i-- {
		var r transcriptRow
		if json.Unmarshal(rows[i], &r) != nil || r.Role != "user" || r.RowID <= 0 ||
			(r.RowKind != "" && r.RowKind != "message") || r.DisplayKind == "hidden" || noteKind(r.DisplayKind) {
			continue
		}
		m := UserMessage{RowID: r.RowID}
		text, images := rowContent(r.Content)
		m.Text, m.Images = text, images
		if r.Text != nil {
			m.Text = *r.Text
		}
		if _, ok := CompactionSummary(m.Text); ok {
			continue
		}
		return m, true
	}
	return UserMessage{}, false
}

// MessageText reads a message's content: a string, or content parts with
// text and images. It returns the text and the number of images.
func MessageText(content json.RawMessage) (string, int) {
	return rowContent(content)
}
