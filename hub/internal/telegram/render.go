package telegram

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
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
	Text    json.RawMessage `json:"text"`
	Status  string          `json:"status"`
	Error   string          `json:"error"`
	HubTurn *hub.TurnTokens `json:"hub_turn"`
}

type approvalPayload struct {
	hub.PromptInfo
	RequestID string `json:"request_id"`
	ID        string `json:"id"`
	ToolName  string `json:"tool_name"`
}

type errorPayload struct {
	Message string `json:"message"`
}

// Values of telegram.steps.
const (
	StepsCollapsed = "collapsed"
	StepsExpanded  = "expanded"
	StepsOff       = "off"
)

// liveMsg is a message that is edited in place, at most once per interval.
type liveMsg struct {
	id       int64
	lastEdit time.Time
	pending  string
	dirty    bool
	// shown is the HTML the message shows and buttons whether it has
	// buttons, so an edit that would change nothing is skipped.
	shown   string
	buttons bool
	// retry is when the chat's budget allows the pending edit.
	retry time.Time
}

// elsewherePrompt is an approval answered where the hub could not see
// the choice.
type elsewherePrompt struct {
	msgID int64
	body  string
	at    time.Time
}

type shownPrompt struct {
	key    string
	msgID  int64
	at     time.Time
	prompt hub.PromptInfo
}

// turnStatus is the status line of one turn: a single message, edited in
// place, that says what Hermes does now and keeps the steps it took.
type turnStatus struct {
	msg       liveMsg
	start     time.Time
	reasoning strings.Builder
	// thinking is the latest line of thinking.delta.
	thinking string
	// tools are the heads of the running tools by ID, and progress the
	// latest progress line of each.
	tools    map[string]string
	progress map[string]string
	running  []string
	steps    []string
	counts   map[string]int
	// tokens are the turn's tokens so far, from the hub's count; nil
	// until the first usage event of the turn.
	tokens *hub.TurnTokens
	// writing is set once the reply streams and no tool runs.
	writing bool
}

// renderer turns one attachment's events into Telegram messages. It is used
// by a single goroutine.
type renderer struct {
	api      *Client
	to       dest
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
	// steps is telegram.steps: how the status line shows the steps.
	steps   string
	approve func(p hub.PromptInfo, body string) (string, func(messageID int64))
	// settle forgets a prompt key that got its answer elsewhere and
	// returns it; false when it is gone already. expire does the same for
	// a prompt whose time ran out.
	settle func(key string) (pendingApproval, bool)
	expire func(key string) (pendingApproval, bool)
	// pending reports whether a prompt of a snapshot still waits; nil
	// when the service cannot say.
	pending func(ctx context.Context, id string) bool
	// recorded finds the answer the hub recorded for an approval; nil
	// when the hub records none.
	recorded func(ctx context.Context, id string) (hub.ApprovalOutcome, bool)
	// elsewhere are the approvals shown as answered elsewhere whose
	// outcome may follow, by prompt ID.
	elsewhere map[string]elsewherePrompt
	// shown reports how many prompts the chat shows.
	shown func(n int)
	// gone reports that Telegram refused a message because the topic is
	// gone.
	gone func()
	// prompts are the shown prompts by ID: their key and message.
	prompts map[string]shownPrompt
	// media opens a file a reply names; nil when the service cannot.
	media func(ctx context.Context, path string) (*hub.MediaFile, error)
	// pageURL is the hub URL that opens a page of the session: a PageID or
	// the path of an HTML file.
	pageURL func(ref string) string
	// deleted reports that the chat of a topic was deleted, so its topic
	// goes too. Nil outside a topic.
	deleted func()
	// stepsSnap keeps the steps so far of the running turn for the Steps
	// button of its status line, and forgets them with "". Nil: no button.
	stepsSnap func(msgID int64, html string)
	// source is the surface that started the chat, "" when unknown.
	source func() string
	// statusTick is how often the status line of a running turn shows the
	// time again when nothing else changed it.
	statusTick time.Duration

	// budget spaces the optional edits of the chat, shared with its other
	// topics; nil spaces none.
	budget *budget
	// drafts streams each turn into a draft (sendMessageDraft), which
	// works only in private chats; the status line then changes only when
	// the turn ends. draftSeq numbers the drafts.
	drafts   bool
	draft    draftState
	draftSeq int64
	// userMsg finds the Telegram message of a prompt the bot sent, by its
	// submission ID; nil finds none.
	userMsg func(id string) (int64, bool)
	// reactTo is the message whose reaction shows how the running turn
	// goes, reacted its reaction, and noReact is set once the chat refused
	// one.
	reactTo  int64
	reacted  string
	noReact  bool
	noTyping bool
	typedAt  time.Time
	// busy is set from a turn's start to its end.
	busy  bool
	todos todoState

	turn   *turnStatus
	answer strings.Builder
	// open is the prompt of the turn a snapshot found running, until the
	// turn's message.start or first step shows it; startedAt is when the
	// hub saw the running turn start, zero when it did not say; echoed is
	// set once the running turn's prompt was shown or skipped.
	open      *hub.OpenPrompt
	startedAt time.Time
	echoed    bool
}

func newRenderer(api *Client, to dest, interval time.Duration, logger *slog.Logger,
	approve func(p hub.PromptInfo, body string) (string, func(messageID int64))) *renderer {
	return &renderer{
		api: api, to: to, interval: interval, logger: logger, now: time.Now, steps: StepsCollapsed,
		approve: approve, prompts: map[string]shownPrompt{}, elsewhere: map[string]elsewherePrompt{},
	}
}

func (r *renderer) handle(ctx context.Context, ev hub.Event) {
	switch ev.Kind {
	case hub.KindSnapshot:
		r.snapshot(ctx, ev)
	case hub.KindReasoningDelta:
		var p streamPayload
		if r.decode(ev, &p) && p.Text != nil {
			r.begin(ctx).reasoning.WriteString(*p.Text)
			r.refresh(ctx)
		}
	case hub.KindThinkingDelta:
		var p streamPayload
		if r.decode(ev, &p) && p.Text != nil && strings.TrimSpace(*p.Text) != "" {
			r.begin(ctx).thinking = truncate(strings.TrimSpace(*p.Text), statusLen)
			r.refresh(ctx)
		}
	case hub.KindToolStart:
		r.toolStart(ctx, ev)
	case hub.KindToolProgress:
		r.toolProgress(ctx, ev)
	case hub.KindToolComplete:
		r.toolComplete(ctx, ev)
	case hub.KindMessageDelta:
		r.delta(ctx, ev)
	case hub.KindMessageStart:
		r.turnStart(ctx, ev)
	case hub.KindMessageComplete:
		r.complete(ctx, ev)
	case usageEvent:
		r.turnUsage(ev)
	case hub.KindApprovalRequest, hub.KindClarifyRequest:
		var p approvalPayload
		if r.decode(ev, &p) {
			r.showPrompt(ctx, ev.Kind, p)
		}
	case "approval.settled", "clarify.settled":
		var p struct {
			PromptID string `json:"prompt_id"`
		}
		if r.decode(ev, &p) {
			r.settled(ctx, false, p.PromptID)
		}
	case "approval.cancelled":
		var p struct {
			RequestIDs []string `json:"request_ids"`
		}
		if r.decode(ev, &p) {
			r.settled(ctx, true, p.RequestIDs...)
		}
	case hub.KindApprovalOutcome:
		var o hub.ApprovalOutcome
		if r.decode(ev, &o) {
			r.approvalOutcome(ctx, o)
		}
	case hub.KindError:
		var p errorPayload
		r.decode(ev, &p)
		r.endTurn(ctx)
		r.endReaction(ctx, reactFailed)
		r.send(ctx, "<b>Error:</b> "+escapeHTML(truncate(p.Message, 3000)), nil)
	case hub.KindSessionDeleted, gateway.UpdatedEvent:
		r.sessionGone(ctx, ev)
	case hub.KindInputNotSent:
		var p hub.HeldInput
		r.decode(ev, &p)
		msg := "<b>Your message was not sent.</b> " + escapeHTML(p.Message)
		if p.Text != "" {
			msg += "\n<i>" + escapeHTML(truncate(p.Text, 1000)) + "</i>"
		}
		r.send(ctx, msg, nil)
	default:
		r.handleMore(ctx, ev)
	}
}

// handleMore handles the events that are not about the turn's messages:
// the to-do list and the answers of /bg prompts.
func (r *renderer) handleMore(ctx context.Context, ev hub.Event) {
	switch ev.Kind {
	case todoEvent:
		var p todoPayload
		if r.decode(ev, &p) {
			r.showTodos(ctx, p)
		}
	case bgCompleteEvent:
		r.backgroundResult(ctx, ev)
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

// snapshot keeps the prompt of the turn a snapshot finds running, and
// shows the prompts of the snapshot that still wait for an answer.
func (r *renderer) snapshot(ctx context.Context, ev hub.Event) {
	var snap hub.Snapshot
	if !r.decode(ev, &snap) {
		return
	}
	if r.open = nil; snap.Running && r.turn == nil && !r.echoed {
		if p, ok := hub.OpenTurnPrompt(snap.Messages); ok {
			r.open = &p
		}
	}
	if r.pending == nil {
		return
	}
	for _, raw := range snap.Prompts {
		var p approvalPayload
		if json.Unmarshal(raw, &p) != nil || p.PromptInfo.ID == "" || !r.pending(ctx, p.PromptInfo.ID) {
			continue
		}
		kind := hub.KindApprovalRequest
		if p.Kind == hub.PromptClarify {
			kind = hub.KindClarifyRequest
		}
		r.showPrompt(ctx, kind, p)
	}
}

// delta keeps a chunk of the reply, and shows on the status line that
// the reply streams.
func (r *renderer) delta(ctx context.Context, ev hub.Event) {
	var p streamPayload
	if !r.decode(ev, &p) || p.Text == nil {
		return
	}
	r.answer.WriteString(*p.Text)
	if *p.Text != "" && r.drafts {
		r.busy = true
		r.markDraft(ctx)
	}
	if t := r.turn; t != nil && *p.Text != "" && !t.writing && len(t.running) == 0 {
		t.writing = true
		r.refresh(ctx)
	}
}

// begin returns the status of the turn that runs, and starts one, after
// the prompt of a turn found running when the chat attached.
func (r *renderer) begin(ctx context.Context) *turnStatus {
	if r.turn == nil {
		r.echoOpen(ctx)
		start := r.now()
		if !r.startedAt.IsZero() && r.startedAt.Before(start) {
			start = r.startedAt
		}
		r.turn = &turnStatus{start: start, tools: map[string]string{}, progress: map[string]string{}, counts: map[string]int{}}
		r.busy = true
	}
	return r.turn
}

// refresh shows the status line of the running turn. Between edits it
// only marks the line dirty; flushDue renders it when the edit is due.
func (r *renderer) refresh(ctx context.Context) {
	t := r.turn
	if t == nil {
		return
	}
	if r.drafts {
		r.running(ctx)
		return
	}
	if !t.msg.lastEdit.IsZero() && r.now().Sub(t.msg.lastEdit) < r.interval {
		t.msg.dirty, t.msg.pending = true, ""
		r.snapSteps()
		return
	}
	r.running(ctx)
}

// running shows the status line of the running turn: one line with the
// Steps button. Telegram folds an expandable quote again on every edit of
// its message, so the steps go to a message of their own on request.
//
// A turn that streams into a draft shows what runs now in the draft; its
// status line is sent once and changes only when the turn ends. Otherwise
// the edit waits when the chat's budget is spent.
func (r *renderer) running(ctx context.Context) {
	t := r.turn
	if r.drafts {
		if t.msg.id == 0 {
			r.update(ctx, &t.msg, workingHTML, r.stepsButton())
		}
		t.msg.dirty, t.msg.pending = false, ""
		r.snapSteps()
		r.markDraft(ctx)
		return
	}
	html, kb := r.statusHTML(false), r.stepsButton()
	if t.msg.id != 0 && r.budget != nil && (html != t.msg.shown || (kb != nil) != t.msg.buttons) {
		if d := r.budget.take(r.to.chat); d > 0 {
			t.msg.dirty, t.msg.retry = true, r.now().Add(d)
			r.snapSteps()
			return
		}
	}
	r.update(ctx, &t.msg, html, kb)
	r.snapSteps()
}

func (r *renderer) stepsButton() *InlineKeyboard {
	if r.stepsSnap == nil || r.steps == StepsOff {
		return nil
	}
	return &InlineKeyboard{InlineKeyboard: [][]InlineButton{{{Text: "Steps", CallbackData: cbSteps}}}}
}

// snapSteps hands the steps so far to the Steps button.
func (r *renderer) snapSteps() {
	t := r.turn
	if r.stepsSnap == nil || r.steps == StepsOff || t == nil || t.msg.id == 0 {
		return
	}
	details := r.details()
	if details == "" {
		details = "No steps yet."
	}
	r.stepsSnap(t.msg.id, "<i>Steps so far ("+duration(r.now().Sub(t.start))+")</i>\n"+r.quote(details))
}

// quote is details in a quote that telegram.steps opens or folds.
func (r *renderer) quote(details string) string {
	if r.steps == StepsExpanded {
		return "<blockquote>" + details + "</blockquote>"
	}
	return "<blockquote expandable>" + details + "</blockquote>"
}

// endTurn shows the status line of the turn as done and forgets the turn.
func (r *renderer) endTurn(ctx context.Context) {
	r.open, r.startedAt, r.echoed = nil, time.Time{}, false
	r.busy = false
	r.draft = draftState{}
	r.endTodo(ctx)
	if r.turn == nil {
		return
	}
	if r.stepsSnap != nil && r.turn.msg.id != 0 {
		r.stepsSnap(r.turn.msg.id, "")
	}
	r.update(ctx, &r.turn.msg, r.statusHTML(true), nil)
	r.turn = nil
}

// stepKind sorts a tool into the kinds of steps the status line counts.
func stepKind(name string) string {
	switch name {
	case "terminal", "execute_code", "process", "shell", "bash":
		return "command"
	case "write_file", "patch", "edit_file":
		return "edit"
	case "read_file":
		return "read"
	case "search_files", "web_search", "web_extract", "session_search":
		return "search"
	}
	return "other"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// summary counts the steps of a turn: "ran 3 commands, edited 1 file".
func (t *turnStatus) summary() string {
	var parts []string
	if n := t.counts["command"]; n > 0 {
		parts = append(parts, "ran "+plural(n, "command", "commands"))
	}
	if n := t.counts["edit"]; n > 0 {
		parts = append(parts, "edited "+plural(n, "file", "files"))
	}
	if n := t.counts["read"]; n > 0 {
		parts = append(parts, "read "+plural(n, "file", "files"))
	}
	if n := t.counts["search"]; n > 0 {
		parts = append(parts, "searched "+plural(n, "time", "times"))
	}
	if n := t.counts["other"]; n > 0 {
		if len(parts) == 0 {
			parts = append(parts, "used "+plural(n, "tool", "tools"))
		} else {
			parts = append(parts, "used "+plural(n, "other tool", "other tools"))
		}
	}
	return strings.Join(parts, ", ")
}

// sessionGone ends the stream of a deleted chat. In a topic it hands the
// topic to the next sync, which deletes it; in a private chat it says so.
func (r *renderer) sessionGone(ctx context.Context, ev hub.Event) {
	if ev.Kind == gateway.UpdatedEvent {
		var p struct {
			Operation string `json:"operation"`
		}
		if !r.decode(ev, &p) || p.Operation != gateway.OpDelete {
			return
		}
	}
	r.endTurn(ctx)
	if r.deleted != nil {
		r.deleted()
		return
	}
	r.send(ctx, "This session was deleted. Send /sessions to pick another one.", nil)
}

// usageEvent is the gateway's mid-turn usage event.
const usageEvent = "session.usage"

// turnUsage takes the turn's tokens from a usage event.
func (r *renderer) turnUsage(ev hub.Event) {
	var p struct {
		HubTurn *hub.TurnTokens `json:"hub_turn"`
	}
	if r.turn != nil && r.decode(ev, &p) && p.HubTurn != nil {
		r.turn.tokens = p.HubTurn
	}
}

// tokenText is " · 31k in, 3k out" for the turn's tokens, " · 3k out"
// when the hub could not count what went in, and "" before any count.
func (t *turnStatus) tokenText() string {
	if t.tokens == nil {
		return ""
	}
	out := tokens(t.tokens.Output) + " out"
	if t.tokens.Input == nil {
		return " · " + out
	}
	return " · " + tokens(*t.tokens.Input) + " in, " + out
}

// duration writes an elapsed time as 12s, 2m 5s or 1h 3m.
func duration(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600 && s%60 == 0:
		return fmt.Sprintf("%dm", s/60)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
}

// statusHTML is the status line. While the turn runs it is one line: what
// runs now, the steps so far, the time and the tokens, such as "Running
// terminal go test ./... · 5 steps · 1m 12s · 14k in, 1k out", with no
// quote, since every edit folds an opened quote again. When final it says
// what the turn did, with the steps and the thinking in a quote that
// telegram.steps opens, folds or leaves out.
func (r *renderer) statusHTML(final bool) string {
	t := r.turn
	elapsed := duration(r.now().Sub(t.start))
	used := t.tokenText()
	if !final {
		var b strings.Builder
		switch {
		case len(t.running) > 0:
			b.WriteString("<b>Running</b> " + t.tools[t.running[len(t.running)-1]])
		case t.writing:
			b.WriteString("<b>Writing…</b>")
		default:
			b.WriteString("<b>Thinking…</b>")
		}
		if n := len(t.steps); n > 0 {
			b.WriteString(" · " + plural(n, "step", "steps"))
		}
		b.WriteString(" · " + elapsed + used)
		return b.String()
	}
	sum := t.summary()
	var b strings.Builder
	if sum == "" {
		b.WriteString("<i>Thought for " + elapsed + used + "</i>")
	} else {
		b.WriteString("<i>Worked for " + elapsed + ": " + escapeHTML(sum) + used + "</i>")
	}
	if details := r.details(); details != "" {
		b.WriteString("\n" + r.quote(details))
	}
	return b.String()
}

// details are the steps of the turn and the end of its thinking, as HTML
// that fits in a message next to the status line.
func (r *renderer) details() string {
	if r.steps == StepsOff {
		return ""
	}
	t := r.turn
	room := reasoningRoom
	var b strings.Builder
	steps := t.steps
	for len(steps) > 0 && utf16Len(strings.Join(steps, "\n")) > room/2 {
		steps = steps[1:]
	}
	if len(steps) < len(t.steps) {
		fmt.Fprintf(&b, "… %d earlier steps\n", len(t.steps)-len(steps))
	}
	b.WriteString(strings.Join(steps, "\n"))
	if text := strings.TrimSpace(t.reasoning.String()); text != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(fitTail(text, room-utf16Len(b.String())))
	}
	return b.String()
}

// fitTail escapes the end of s so the result fits in room.
func fitTail(s string, room int) string {
	n := max(room, 1)
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

// update shows html in m: a new message when m has none yet, an edit when
// the interval allows or force is set, and otherwise a pending edit for
// flushDue.
// update sends or edits a live message with the buttons of kb; an edit
// without kb takes the buttons away.
func (r *renderer) update(ctx context.Context, m *liveMsg, html string, kb *InlineKeyboard) {
	if m.id == 0 {
		m.id = r.send(ctx, html, kb)
		m.pending = ""
		m.lastEdit = r.now()
		m.dirty = m.id == 0
		m.shown, m.buttons = html, kb != nil
		return
	}
	r.edit(ctx, m, html, kb)
}

func (r *renderer) edit(ctx context.Context, m *liveMsg, html string, kb *InlineKeyboard) {
	m.dirty = false
	m.pending = ""
	m.lastEdit = r.now()
	if html == m.shown && (kb != nil) == m.buttons {
		return
	}
	r.spend()
	if err := r.api.EditHTML(ctx, r.to.chat, m.id, html, kb); err != nil {
		r.logger.Warn("Failed to edit message", "message", m.id, "error", err)
		return
	}
	m.shown, m.buttons = html, kb != nil
}

// spend records a message or edit in the chat's budget.
func (r *renderer) spend() {
	if r.budget != nil {
		r.budget.spend(r.to.chat)
	}
}

// expirySlack is how long after a prompt's timeout the bot marks it
// expired, so Hermes's own settle comes first when it can.
const expirySlack = 5 * time.Second

// deadline is when a shown prompt expires, false for one without a
// timeout.
func (sp shownPrompt) deadline() (time.Time, bool) {
	if sp.prompt.Timeout <= 0 {
		return time.Time{}, false
	}
	timeout := time.Duration(sp.prompt.Timeout * float64(time.Second))
	return sp.at.Add(timeout + slack(timeout)), true
}

// slack is how long after a timeout the bot marks a prompt expired:
// expirySlack, or a tenth of a short timeout.
func slack(timeout time.Duration) time.Duration {
	return min(expirySlack, timeout/10)
}

// nextFlush returns how long until the next pending edit or prompt
// expiry is due.
func (r *renderer) nextFlush() (time.Duration, bool) {
	var best time.Duration
	found := false
	consider := func(d time.Duration) {
		d = max(d, 0)
		if !found || d < best {
			best, found = d, true
		}
	}
	if t := r.turn; t != nil && !r.drafts && t.msg.dirty {
		at := t.msg.lastEdit.Add(r.interval)
		if t.msg.retry.After(at) {
			at = t.msg.retry
		}
		consider(at.Sub(r.now()))
	} else if t != nil && !r.drafts && t.msg.id != 0 {
		consider(t.msg.lastEdit.Add(r.tick()).Sub(r.now()))
	}
	if m := r.todos.msg; m.dirty && m.id != 0 {
		consider(m.retry.Sub(r.now()))
	}
	if d, ok := r.draftDue(); ok {
		consider(d)
	}
	if d, ok := r.typingDue(); ok {
		consider(d)
	}
	for _, sp := range r.prompts {
		if at, ok := sp.deadline(); ok {
			consider(at.Sub(r.now()))
		}
	}
	return best, found
}

// statusTick is how often the status line of a running turn shows the
// time again when nothing else changed it.
const statusTick = 10 * time.Second

func (r *renderer) tick() time.Duration {
	return max(r.interval, cmp.Or(r.statusTick, statusTick))
}

// flushDue renders the status line when an edit is pending and due, or
// when it has not changed for a tick, so its time keeps going while the
// model thinks or writes.
func (r *renderer) flushDue(ctx context.Context) {
	r.typingTick(ctx)
	r.flushTodo(ctx, false)
	if r.drafts {
		r.draftTick(ctx)
		return
	}
	t := r.turn
	if t == nil || t.msg.id == 0 && !t.msg.dirty {
		return
	}
	since := r.now().Sub(t.msg.lastEdit)
	if t.msg.dirty && since >= r.interval && !r.now().Before(t.msg.retry) || since >= r.tick() {
		r.running(ctx)
	}
}

func (r *renderer) flushAll(ctx context.Context) {
	r.flushTodo(ctx, true)
	if t := r.turn; t != nil && t.msg.dirty && !r.drafts {
		r.running(ctx)
	}
}

// expireDue marks the prompts whose time ran out as expired.
func (r *renderer) expireDue(ctx context.Context) {
	for id, sp := range r.prompts {
		at, ok := sp.deadline()
		if !ok || r.now().Before(at) {
			continue
		}
		delete(r.prompts, id)
		if r.expire == nil {
			continue
		}
		if p, ok := r.expire(sp.key); ok && sp.msgID != 0 {
			r.outcome(ctx, sp.msgID, "Expired", p.body, "No answer in "+duration(time.Duration(sp.prompt.Timeout*float64(time.Second)))+".")
		}
	}
	r.reportShown()
	r.reactNeeds(ctx)
}

func (r *renderer) reportShown() {
	if r.shown != nil {
		r.shown(len(r.prompts))
	}
}

// outcome shows what became of a prompt in its message and takes its
// buttons off.
func (r *renderer) outcome(ctx context.Context, msgID int64, head, body, note string) {
	r.spend()
	if err := r.api.EditHTML(ctx, r.to.chat, msgID, outcomeHTML(head, body, note), nil); err != nil {
		r.logger.Warn("Failed to show a prompt's outcome", "error", err)
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
	t := r.begin(ctx)
	t.writing = false
	if _, ok := t.tools[p.ToolID]; !ok {
		t.running = append(t.running, p.ToolID)
	}
	t.tools[p.ToolID] = toolHead(p)
	r.refresh(ctx)
}

func (r *renderer) toolProgress(ctx context.Context, ev hub.Event) {
	var p toolPayload
	if !r.decode(ev, &p) || r.turn == nil {
		return
	}
	if _, ok := r.turn.tools[p.ToolID]; !ok {
		return
	}
	line := strings.TrimSpace(p.Message)
	if p.Percent != nil {
		line = strings.TrimSpace(fmt.Sprintf("%s %d%%", line, *p.Percent))
	}
	if line == "" {
		return
	}
	r.turn.progress[p.ToolID] = truncate(line, statusLen)
	r.refresh(ctx)
}

func (r *renderer) toolComplete(ctx context.Context, ev hub.Event) {
	var p toolPayload
	if !r.decode(ev, &p) {
		return
	}
	t := r.begin(ctx)
	head, ok := t.tools[p.ToolID]
	if !ok {
		head = toolHead(p)
	}
	delete(t.tools, p.ToolID)
	delete(t.progress, p.ToolID)
	t.running = slices.DeleteFunc(t.running, func(id string) bool { return id == p.ToolID })
	step := "✓ " + head
	if p.DurationS != nil {
		step += fmt.Sprintf(" <i>(%.1fs)</i>", *p.DurationS)
	}
	if s := strings.TrimSpace(p.Summary); s != "" {
		step += " " + escapeHTML(truncate(strings.Join(strings.Fields(s), " "), statusLen))
	}
	t.steps = append(t.steps, step)
	t.counts[stepKind(p.Name)]++
	r.refresh(ctx)
}

// showPrompt sends an approval or a question with its buttons. A question
// without choices asks for a reply to its message instead.
func (r *renderer) showPrompt(ctx context.Context, kind string, p approvalPayload) {
	info := p.PromptInfo
	for _, id := range []string{p.RequestID, p.ID} {
		if info.ID == "" {
			info.ID = id
		}
	}
	if kind == hub.KindClarifyRequest {
		info.Kind = hub.PromptClarify
	} else {
		info.Kind = hub.PromptApproval
	}
	if _, ok := r.prompts[info.ID]; ok || info.ID == "" {
		return
	}
	r.flushAll(ctx)
	head := "<b>Question</b>"
	if info.Kind == hub.PromptApproval {
		head = "<b>Needs you</b>"
		if p.ToolName != "" {
			head += " <i>(" + escapeHTML(p.ToolName) + ")</i>"
		}
	}
	body := promptBody(&info)
	key, bind := r.approve(info, body)
	text := head + "\n" + body + promptFooter(info)
	var id int64
	if info.Kind == hub.PromptClarify && len(info.Choices) == 0 {
		var err error
		id, err = r.api.SendForceReply(ctx, r.to.chat, r.to.thread, text, "Your answer")
		r.sendFailed(err)
		r.spend()
		r.draftLost()
	} else {
		id = r.send(ctx, text, promptKeyboard(cbApproval, key, info, nil))
	}
	bind(id)
	r.prompts[info.ID] = shownPrompt{key: key, msgID: id, at: r.now(), prompt: info}
	r.reportShown()
	r.reactNeeds(ctx)
}

// settled shows the outcome of shown prompts that got their answer, or
// were cancelled, elsewhere.
func (r *renderer) settled(ctx context.Context, cancelled bool, ids ...string) {
	for _, id := range ids {
		sp, ok := r.prompts[id]
		if !ok {
			continue
		}
		delete(r.prompts, id)
		if r.settle == nil {
			continue
		}
		if p, ok := r.settle(sp.key); ok && sp.msgID != 0 {
			var answer func() (hub.ApprovalOutcome, bool)
			if r.recorded != nil {
				answer = func() (hub.ApprovalOutcome, bool) { return r.recorded(ctx, id) }
			}
			head, note, final := settledOutcome(sp.prompt, sp.at, r.now(), cancelled, answer)
			r.outcome(ctx, sp.msgID, head, p.body, note)
			if !final {
				r.waitOutcome(id, elsewherePrompt{msgID: sp.msgID, body: p.body, at: r.now()})
			}
		}
	}
	r.reportShown()
	r.reactNeeds(ctx)
}

// waitOutcome keeps an approval answered elsewhere until its outcome
// follows, and forgets those older than keepShown.
func (r *renderer) waitOutcome(id string, ep elsewherePrompt) {
	for k, e := range r.elsewhere {
		if ep.at.Sub(e.at) > keepShown {
			delete(r.elsewhere, k)
		}
	}
	r.elsewhere[id] = ep
}

// approvalOutcome shows what became of an approval answered elsewhere.
func (r *renderer) approvalOutcome(ctx context.Context, o hub.ApprovalOutcome) {
	ep, ok := r.elsewhere[o.PromptID]
	if !ok {
		return
	}
	delete(r.elsewhere, o.PromptID)
	r.outcome(ctx, ep.msgID, outcomeHead(o), ep.body, "")
}

func (r *renderer) complete(ctx context.Context, ev hub.Event) {
	var p completePayload
	r.decode(ev, &p)
	if r.turn != nil && p.HubTurn != nil {
		r.turn.tokens = p.HubTurn
	}
	r.endTurn(ctx)
	switch p.Status {
	case "interrupted":
		r.endReaction(ctx, reactStopped)
	case "error":
		r.endReaction(ctx, reactFailed)
	default:
		r.endReaction(ctx, reactDone)
	}
	text := r.answer.String()
	var s string
	if err := json.Unmarshal(p.Text, &s); err == nil && strings.TrimSpace(s) != "" {
		text = s
	}
	r.answer.Reset()
	pages, text := hub.PageBlocks(text)
	paths, text := hub.MediaDirectives(text)
	switch p.Status {
	case "interrupted":
		text += "\n\n**Interrupted.**"
	case "error":
		if p.Error != "" {
			text += "\n\nError: " + p.Error
		}
	}
	if strings.TrimSpace(text) != "" {
		for _, chunk := range RenderMarkdown(text, MaxMessageLen) {
			if r.send(ctx, chunk, nil) == 0 {
				break
			}
		}
	}
	r.sendPages(ctx, pages)
	r.sendMedia(ctx, paths)
}

// cronResult sends the text of a cron result Hermes delivered into the
// session. A cron result is a note, not a turn, so no reply follows it.
func (r *renderer) cronResult(ctx context.Context, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, chunk := range RenderMarkdown(text, MaxMessageLen) {
		if r.send(ctx, chunk, nil) == 0 {
			break
		}
	}
}

// agentMessage shows a message that another agent sent this chat through
// the hub, which starts its turn.
func (r *renderer) agentMessage(ctx context.Context, text string) {
	m := hub.AgentHeaderRE.FindStringSubmatch(text)
	if m == nil {
		return
	}
	body := text[len(m[0]):]
	if _, rest, ok := strings.Cut(body, "\n\n"); ok {
		body = rest
	}
	head := "<b>Agent message from " + escapeHTML(m[2]) + "</b>\n"
	for i, chunk := range RenderMarkdown(body, MaxMessageLen-len(head)) {
		if i == 0 {
			chunk = head + chunk
		}
		if r.send(ctx, chunk, nil) == 0 {
			break
		}
	}
}

// send sends html and returns the message ID, or 0 on failure. When Telegram
// rejects the markup it resends the text without formatting.
func (r *renderer) send(ctx context.Context, html string, kb *InlineKeyboard) int64 {
	id, err := sendHTMLErr(ctx, r.api, r.logger, r.to, html, kb)
	r.sendFailed(err)
	r.spend()
	r.draftLost()
	return id
}

// sendFailed reports a topic that Telegram says is gone.
func (r *renderer) sendFailed(err error) {
	var apiErr *APIError
	if r.to.thread != 0 && r.gone != nil && errors.As(err, &apiErr) && apiErr.TopicGone() {
		r.gone()
	}
}
