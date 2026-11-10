package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	reasoningRoom  = 3500
	argsPreviewLen = 200
	resultLen      = 1000
	statusLen      = 200
)

type streamPayload struct {
	Text *string `json:"text"`
}

type toolPayload struct {
	ToolID     string          `json:"tool_id"`
	Name       string          `json:"name"`
	Context    string          `json:"context"`
	Args       json.RawMessage `json:"args"`
	ArgsText   string          `json:"args_text"`
	Preview    string          `json:"preview"`
	DurationS  *float64        `json:"duration_s"`
	Summary    string          `json:"summary"`
	ResultText string          `json:"result_text"`
	Percent    *int            `json:"percent"`
	Message    string          `json:"message"`
}

type completePayload struct {
	Text   json.RawMessage `json:"text"`
	Status string          `json:"status"`
	Error  string          `json:"error"`
}

type approvalPayload struct {
	PromptID    string `json:"prompt_id"`
	RequestID   string `json:"request_id"`
	ID          string `json:"id"`
	Command     string `json:"command"`
	Description string `json:"description"`
	ToolName    string `json:"tool_name"`
}

type errorPayload struct {
	Message string `json:"message"`
}

// liveMsg is a message that is edited in place, at most once per interval.
type liveMsg struct {
	id       int64
	lastEdit time.Time
	pending  string
	dirty    bool
}

type toolMsg struct {
	liveMsg
	head string
}

// renderer turns one attachment's events into Telegram messages. It is used
// by a single goroutine.
type renderer struct {
	api      *Client
	chatID   int64
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
	approve  func(approvalID, text string) (string, func(messageID int64))

	think      *liveMsg
	thinkStart time.Time
	reasoning  strings.Builder
	status     string

	tools  map[string]*toolMsg
	answer strings.Builder
}

func newRenderer(api *Client, chatID int64, interval time.Duration, logger *slog.Logger,
	approve func(approvalID, text string) (string, func(messageID int64))) *renderer {
	return &renderer{
		api: api, chatID: chatID, interval: interval, logger: logger, now: time.Now,
		approve: approve, tools: make(map[string]*toolMsg),
	}
}

func (r *renderer) handle(ctx context.Context, ev hub.Event) {
	switch ev.Kind {
	case hub.KindReasoningDelta:
		var p streamPayload
		if r.decode(ev, &p) && p.Text != nil {
			r.startThinking()
			r.reasoning.WriteString(*p.Text)
			r.update(ctx, r.think, r.thinkingHTML(false), false)
		}
	case hub.KindThinkingDelta:
		var p streamPayload
		if r.decode(ev, &p) && p.Text != nil && strings.TrimSpace(*p.Text) != "" {
			r.startThinking()
			r.status = truncate(strings.TrimSpace(*p.Text), statusLen)
			r.update(ctx, r.think, r.thinkingHTML(false), false)
		}
	case hub.KindToolStart:
		r.toolStart(ctx, ev)
	case hub.KindToolProgress:
		r.toolProgress(ctx, ev)
	case hub.KindToolComplete:
		r.toolComplete(ctx, ev)
	case hub.KindMessageDelta:
		var p streamPayload
		if r.decode(ev, &p) && p.Text != nil {
			r.answer.WriteString(*p.Text)
		}
	case hub.KindMessageComplete:
		r.complete(ctx, ev)
	case hub.KindApprovalRequest:
		r.approval(ctx, ev)
	case hub.KindError:
		var p errorPayload
		r.decode(ev, &p)
		r.sealThinking(ctx)
		r.send(ctx, "<b>Error:</b> "+escapeHTML(truncate(p.Message, 3000)), nil)
	case hub.KindSessionDeleted:
		r.sealThinking(ctx)
		r.send(ctx, "This session was deleted. Send /sessions to pick another one.", nil)
	default:
		r.logger.Debug("Ignoring event", "kind", ev.Kind, "seq", ev.Seq)
	}
}

func (r *renderer) decode(ev hub.Event, v any) bool {
	if len(ev.Data) == 0 {
		return true
	}
	if err := json.Unmarshal(ev.Data, v); err != nil {
		r.logger.Warn("Failed to decode event", "kind", ev.Kind, "seq", ev.Seq, "error", err)
		return false
	}
	return true
}

func (r *renderer) startThinking() {
	if r.think == nil {
		r.think = &liveMsg{}
		r.thinkStart = r.now()
		r.reasoning.Reset()
		r.status = ""
	}
}

func (r *renderer) thinkingHTML(final bool) string {
	var b strings.Builder
	if final {
		secs := int(r.now().Sub(r.thinkStart).Round(time.Second) / time.Second)
		fmt.Fprintf(&b, "<i>Thought for %ds</i>", secs)
	} else {
		b.WriteString("<b>Thinking…</b>")
		if r.status != "" {
			b.WriteString("\n<i>" + escapeHTML(r.status) + "</i>")
		}
	}
	if text := strings.TrimSpace(r.reasoning.String()); text != "" {
		b.WriteString("\n<blockquote expandable>" + fitTail(text, reasoningRoom) + "</blockquote>")
	}
	return b.String()
}

// fitTail escapes the end of s so the result fits in room.
func fitTail(s string, room int) string {
	n := room
	for {
		t, cut := tail(s, n)
		e := escapeHTML(t)
		if cut {
			e = "…" + e
		}
		if utf16Len(e) <= room || n <= 1 {
			return e
		}
		n = n * 3 / 4
	}
}

func (r *renderer) sealThinking(ctx context.Context) {
	if r.think == nil {
		return
	}
	r.update(ctx, r.think, r.thinkingHTML(true), true)
	r.think = nil
}

// update shows html in m: a new message when m has none yet, an edit when
// the interval allows or force is set, and otherwise a pending edit for
// flushDue.
func (r *renderer) update(ctx context.Context, m *liveMsg, html string, force bool) {
	if m.id == 0 {
		m.id = r.send(ctx, html, nil)
		m.lastEdit = r.now()
		m.dirty = false
		return
	}
	if !force && r.now().Sub(m.lastEdit) < r.interval {
		m.pending, m.dirty = html, true
		return
	}
	r.edit(ctx, m, html)
}

func (r *renderer) edit(ctx context.Context, m *liveMsg, html string) {
	m.dirty = false
	m.pending = ""
	m.lastEdit = r.now()
	if err := r.api.EditHTML(ctx, r.chatID, m.id, html, nil); err != nil {
		r.logger.Warn("Failed to edit message", "chat", r.chatID, "message", m.id, "error", err)
	}
}

func (r *renderer) live() []*liveMsg {
	out := make([]*liveMsg, 0, len(r.tools)+1)
	if r.think != nil {
		out = append(out, r.think)
	}
	for _, t := range r.tools {
		out = append(out, &t.liveMsg)
	}
	return out
}

// nextFlush returns how long until the next pending edit is due.
func (r *renderer) nextFlush() (time.Duration, bool) {
	var best time.Duration
	found := false
	for _, m := range r.live() {
		if !m.dirty {
			continue
		}
		d := max(m.lastEdit.Add(r.interval).Sub(r.now()), 0)
		if !found || d < best {
			best, found = d, true
		}
	}
	return best, found
}

func (r *renderer) flushDue(ctx context.Context) {
	for _, m := range r.live() {
		if m.dirty && r.now().Sub(m.lastEdit) >= r.interval {
			r.edit(ctx, m, m.pending)
		}
	}
}

func (r *renderer) flushAll(ctx context.Context) {
	for _, m := range r.live() {
		if m.dirty {
			r.edit(ctx, m, m.pending)
		}
	}
}

func toolArgs(p toolPayload) string {
	for _, s := range []string{p.Context, p.Preview, p.ArgsText} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	if len(p.Args) > 0 && string(p.Args) != "null" && string(p.Args) != "{}" {
		return string(p.Args)
	}
	return ""
}

func toolHead(p toolPayload) string {
	name := p.Name
	if name == "" {
		name = "tool"
	}
	head := "<b>" + escapeHTML(name) + "</b>"
	if args := toolArgs(p); args != "" {
		head += " <code>" + escapeHTML(truncate(strings.Join(strings.Fields(args), " "), argsPreviewLen)) + "</code>"
	}
	return head
}

func (r *renderer) toolStart(ctx context.Context, ev hub.Event) {
	var p toolPayload
	if !r.decode(ev, &p) {
		return
	}
	r.sealThinking(ctx)
	t := &toolMsg{head: toolHead(p)}
	r.tools[p.ToolID] = t
	r.update(ctx, &t.liveMsg, "▶ "+t.head, true)
}

func (r *renderer) toolProgress(ctx context.Context, ev hub.Event) {
	var p toolPayload
	if !r.decode(ev, &p) {
		return
	}
	t, ok := r.tools[p.ToolID]
	if !ok {
		return
	}
	line := strings.TrimSpace(p.Message)
	if p.Percent != nil {
		line = strings.TrimSpace(fmt.Sprintf("%s %d%%", line, *p.Percent))
	}
	if line == "" {
		return
	}
	r.update(ctx, &t.liveMsg, "▶ "+t.head+"\n<i>"+escapeHTML(truncate(line, statusLen))+"</i>", false)
}

func (r *renderer) toolComplete(ctx context.Context, ev hub.Event) {
	var p toolPayload
	if !r.decode(ev, &p) {
		return
	}
	t, ok := r.tools[p.ToolID]
	if !ok {
		t = &toolMsg{head: toolHead(p)}
	}
	delete(r.tools, p.ToolID)
	html := "✓ " + t.head
	if p.DurationS != nil {
		html += fmt.Sprintf(" <i>(%.1fs)</i>", *p.DurationS)
	}
	if s := strings.TrimSpace(p.Summary); s != "" {
		html += "\n" + escapeHTML(truncate(s, statusLen))
	} else if s := strings.TrimSpace(p.ResultText); s != "" {
		html += "\n<blockquote expandable>" + escapeHTML(truncate(s, resultLen)) + "</blockquote>"
	}
	r.update(ctx, &t.liveMsg, html, true)
}

func (r *renderer) approval(ctx context.Context, ev hub.Event) {
	var p approvalPayload
	if !r.decode(ev, &p) {
		return
	}
	id := p.PromptID
	if id == "" {
		id = p.RequestID
	}
	if id == "" {
		id = p.ID
	}
	r.sealThinking(ctx)
	var b strings.Builder
	b.WriteString("<b>Approval needed</b>")
	if p.ToolName != "" {
		b.WriteString(" for <b>" + escapeHTML(p.ToolName) + "</b>")
	}
	if p.Command != "" {
		b.WriteString("\n<pre>" + escapeHTML(truncate(p.Command, 2500)) + "</pre>")
	}
	if p.Description != "" {
		b.WriteString("\n" + escapeHTML(truncate(p.Description, 1000)))
	}
	text := b.String()
	key, bind := r.approve(id, text)
	kb := &InlineKeyboard{InlineKeyboard: [][]InlineButton{{
		{Text: "Allow", CallbackData: "ap:" + key + ":1"},
		{Text: "Deny", CallbackData: "ap:" + key + ":0"},
	}}}
	bind(r.send(ctx, text, kb))
}

func (r *renderer) complete(ctx context.Context, ev hub.Event) {
	var p completePayload
	r.decode(ev, &p)
	r.sealThinking(ctx)
	r.flushAll(ctx)
	text := r.answer.String()
	var s string
	if err := json.Unmarshal(p.Text, &s); err == nil && strings.TrimSpace(s) != "" {
		text = s
	}
	r.answer.Reset()
	r.tools = make(map[string]*toolMsg)
	switch p.Status {
	case "interrupted":
		text += "\n\n**Interrupted.**"
	case "error":
		if p.Error != "" {
			text += "\n\nError: " + p.Error
		}
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, chunk := range RenderMarkdown(text, MaxMessageLen) {
		if r.send(ctx, chunk, nil) == 0 {
			break
		}
	}
}

// send sends html and returns the message ID, or 0 on failure. When Telegram
// rejects the markup it resends the text without formatting.
func (r *renderer) send(ctx context.Context, html string, kb *InlineKeyboard) int64 {
	return sendHTML(ctx, r.api, r.logger, r.chatID, html, kb)
}
