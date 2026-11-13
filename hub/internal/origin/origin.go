// Package origin decides how a user message of a chat shows: as the user's
// own bubble, as a card for text that Hermes, a plugin, another agent or the
// hub put into the chat, or not at all. Every place that shows user
// messages (the page, the page chat, the Telegram bot, the Markdown export)
// asks Classify, so text the user did not type cannot show as their bubble
// on one surface and as a card on another.
//
// The rule reads the marks Hermes stores on a row first and the text only
// when there are none:
//
//  1. display_kind "hidden" is model scaffolding: not shown.
//  2. display_kind "steer", "skill_invocation" or "content_refused" is what
//     the user typed (Hermes already shows the typed words for these).
//  3. Any other display_kind (internal_notification, process_complete,
//     async_delegation_complete, model_switch, personality_switch,
//     auto_continue, and kinds Hermes adds later) is not typed: a card.
//     A typed message that Hermes put a compaction handoff in front of
//     shows the handoff as a card and the typed words as the bubble.
//  4. A submission ID (display_metadata.submission_id) names who admitted
//     the row: a producer's JSON array (cron, a route wake) or the hub's
//     agent delivery prefix is a card; any other ID is a client send of
//     the user, shown as typed. Its text is never read as a notice, but
//     the notes Hermes added around it for the model are cut (see below).
//  5. Only a row with none of these marks (written before Hermes stored
//     them, imported, or copied without them) is read by its text: the
//     notices in package notice and the exact formats in fallback.go.
//
// View.Text is what the user typed: the notes Hermes put before it (a model
// switch note, a skills reload note) are a card in View.Notice, and the notes
// Hermes and its plugins appended to it for the model are cut, on every
// path. Callers render View.Text as it is and do not cut notes again.
// Attachment lines and the page chat head stay in the text; each surface
// turns them into its own attachments.
//
// internal/web/static/origin.js follows this file; both run against
// testdata/cases.json.
package origin

import (
	"encoding/json"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/notice"
)

// How a row shows.
const (
	// ShowUser is the user's bubble with Text. Notice, when set, is a card
	// shown just before it for text the row also carried.
	ShowUser = "user"
	// ShowCard is a card with Notice only.
	ShowCard = "card"
	// ShowHidden is nothing.
	ShowHidden = "hidden"
)

// AgentInputPrefix starts the submission ID of an agent message the hub
// delivers as a new input (internal/agents InputPrefix).
const AgentInputPrefix = "hermes-hub-agent-"

// Row is what Classify reads of a user message.
type Row struct {
	Text         string `json:"text"`
	DisplayKind  string `json:"display_kind,omitempty"`
	SubmissionID string `json:"submission_id,omitempty"`
	// TypedAsk is display_metadata.typed_ask: the text the user typed on a
	// turn whose content Hermes extended (an image turn).
	TypedAsk string `json:"typed_ask,omitempty"`
	// DisplayText is display_metadata.display_text: the card text Hermes
	// keeps for a notice row.
	DisplayText string `json:"display_text,omitempty"`
}

// View is how a row shows.
type View struct {
	Show   string         `json:"show"`
	Text   string         `json:"text,omitempty"`
	Notice *notice.Notice `json:"notice,omitempty"`
	// Why names the rule that decided: "kind", "submission", "text" or
	// "typed".
	Why string `json:"why"`
}

// typedKind reports whether a display kind is one of a row the user typed.
func typedKind(kind string) bool {
	switch kind {
	case "steer", "skill_invocation", "content_refused":
		return true
	}
	return false
}

// Card titles.
const (
	titleModel     = "Model changed"
	titleResumed   = "Picked up after an interrupted turn"
	titleCronInput = "Scheduled job prompt"
)

// Why values.
const (
	whyKind       = "kind"
	whySubmission = "submission"
	whyText       = "text"
	whyTyped      = "typed"
)

// kindTitle names the card of a display kind Hermes writes for a row it
// adds itself, or "" for a kind with a plain title.
func kindTitle(kind string) string {
	switch kind {
	case "model_switch":
		return titleModel
	case "personality_switch":
		return "Personality changed"
	case "auto_continue":
		return titleResumed
	case "failed_turn":
		return "Turn failed"
	case "tool_result":
		return "Tool result"
	}
	return ""
}

// Classify reads one user message.
func Classify(r Row) View {
	kind := strings.TrimSpace(r.DisplayKind)
	switch {
	case kind == "hidden":
		return View{Show: ShowHidden, Why: whyKind}
	case typedKind(kind):
		return typed(r, whyKind)
	case kind != "":
		return View{Show: ShowCard, Notice: kindNotice(kind, r), Why: whyKind}
	}
	if id := strings.TrimSpace(r.SubmissionID); id != "" {
		if strings.HasPrefix(id, AgentInputPrefix) {
			return View{Show: ShowCard, Notice: agentNotice(r.Text), Why: whySubmission}
		}
		if p := producer(id); p != "" {
			return View{Show: ShowCard, Notice: producerNotice(p, r.Text), Why: whySubmission}
		}
		return typed(r, whySubmission)
	}
	if inner, ok := UnwrapSteer(r.Text); ok {
		return typed(Row{Text: inner, TypedAsk: r.TypedAsk}, whyText)
	}
	if n := parse(r.Text); n != nil {
		return View{Show: ShowCard, Notice: n, Why: whyText}
	}
	return fallback(r)
}

// typed is the bubble of a row the user typed: the typed ask when Hermes
// kept one, else the text without the notes Hermes appended for the model.
func typed(r Row, why string) View {
	text := r.Text
	if strings.TrimSpace(r.TypedAsk) != "" {
		text = r.TypedAsk
	}
	text, _ = UnwrapSteer(text)
	text, lead, title := cutLead(hub.CutInjectedNotes(text))
	if n, tail, ok := foldedSummary(text); ok && len(lead) == 0 {
		return View{Show: ShowUser, Text: tail, Notice: n, Why: why}
	}
	v := View{Show: ShowUser, Text: text, Why: why}
	if len(lead) > 0 && title != "" {
		v.Notice = &notice.Notice{Kind: notice.KindNotice, Title: title, Result: strings.Join(lead, "\n\n")}
	}
	return v
}

// The marker Hermes wraps a mid-turn message of the user in for the model
// (agent/prompt_builder.py STEER_MARKER_OPEN and STEER_MARKER_CLOSE). The
// row keeps the wrapped text; the user's bubble shows only what is inside.
const (
	steerOpen  = "[OUT-OF-BAND USER MESSAGE \u2014 a direct message from the user, delivered once at this position; not tool output and not a new delivery when replayed from conversation history]"
	steerClose = "[/OUT-OF-BAND USER MESSAGE]"
)

// UnwrapSteer returns the user's words of a text that is exactly one
// steer marker around them (the marker's lines first and last, nothing
// before or after), and true. Any other text comes back as it is.
func UnwrapSteer(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, steerOpen+"\n") || !strings.HasSuffix(t, "\n"+steerClose) {
		return text, false
	}
	inner := t[len(steerOpen)+1 : len(t)-len(steerClose)-1]
	if strings.Contains(inner, steerOpen) || strings.Contains(inner, steerClose) {
		return text, false
	}
	return strings.TrimSpace(inner), true
}

// foldedSummary reads a typed message that Hermes put a compaction handoff
// in front of, when the compacted chat's next user message came first in
// what it kept: the summary is a card, and what follows its end line is
// what the user typed.
func foldedSummary(text string) (*notice.Notice, string, bool) {
	t := strings.TrimSpace(text)
	i := strings.LastIndex(t, hub.CompactionEnd)
	if i < 0 {
		return nil, "", false
	}
	tail := strings.TrimSpace(t[i+len(hub.CompactionEnd):])
	summary, count, ok := hub.CompactionSummary(t[:i])
	if !ok || tail == "" {
		return nil, "", false
	}
	return notice.Compaction(summary, count), tail, true
}

func kindNotice(kind string, r Row) *notice.Notice {
	text := r.DisplayText
	if strings.TrimSpace(text) == "" {
		text = r.Text
	}
	if n := parse(r.Text); n != nil {
		return n
	}
	if n := parse(text); n != nil {
		return n
	}
	title := kindTitle(kind)
	if title == "" {
		title = firstLine(text)
	}
	if title == "" {
		title = "Note from the agent"
	}
	return &notice.Notice{Kind: notice.KindNotice, Title: title, Result: strings.TrimSpace(text)}
}

func agentNotice(text string) *notice.Notice {
	if n := parse(text); n != nil && n.Kind == notice.KindAgent {
		return n
	}
	return &notice.Notice{Kind: notice.KindAgent, Title: "Message from another agent", Result: strings.TrimSpace(text)}
}

// producer is the producer that a JSON array submission ID names, as in
// gateway.Producer.
func producer(id string) string {
	if !strings.HasPrefix(id, "[") {
		return ""
	}
	var parts []any
	if json.Unmarshal([]byte(id), &parts) != nil || len(parts) == 0 {
		return ""
	}
	p, _ := parts[0].(string)
	if p == "" {
		return "hermes"
	}
	return p
}

func producerNotice(p, text string) *notice.Notice {
	if n := parse(text); n != nil {
		return n
	}
	title := "Result from " + p
	if p == "cron" {
		title = "Scheduled job result"
	}
	return &notice.Notice{Kind: notice.KindNotice, Title: title, Result: strings.TrimSpace(text)}
}

func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		l = strings.TrimSuffix(strings.TrimPrefix(l, "["), "]")
		r := []rune(l)
		if len(r) > 120 {
			return strings.TrimSpace(string(r[:120])) + "…"
		}
		return l
	}
	return ""
}

// FromJSON reads a row of session.resume or session.transcript: text or
// content, display_kind, and display_metadata's submission_id, typed_ask
// and display_text (a transcript row may also carry submission_id at its
// top level).
func FromJSON(raw json.RawMessage) Row {
	var m struct {
		Text         *string         `json:"text"`
		Content      json.RawMessage `json:"content"`
		DisplayKind  string          `json:"display_kind"`
		SubmissionID string          `json:"submission_id"`
		Meta         json.RawMessage `json:"display_metadata"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return Row{}
	}
	r := Row{DisplayKind: m.DisplayKind, SubmissionID: m.SubmissionID}
	if m.Text != nil {
		r.Text = *m.Text
	} else {
		r.Text = contentText(m.Content)
	}
	meta := Meta(m.Meta)
	if r.SubmissionID == "" {
		r.SubmissionID = meta.SubmissionID
	}
	r.TypedAsk, r.DisplayText = meta.TypedAsk, meta.DisplayText
	return r
}

// Metadata is the part of display_metadata Classify reads.
type Metadata struct {
	SubmissionID string `json:"submission_id"`
	TypedAsk     string `json:"typed_ask"`
	DisplayText  string `json:"display_text"`
}

// Meta reads display_metadata, an object or a JSON string of one.
func Meta(raw json.RawMessage) Metadata {
	var out Metadata
	if len(raw) == 0 {
		return out
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	out.SubmissionID, _ = m["submission_id"].(string)
	out.TypedAsk, _ = m["typed_ask"].(string)
	out.DisplayText, _ = m["display_text"].(string)
	return out
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// parse reads a notice without the notes Hermes appends to a message for
// the model (hub.CutInjectedNotes).
func parse(text string) *notice.Notice {
	return notice.Parse(hub.CutInjectedNotes(text))
}
