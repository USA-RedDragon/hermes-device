package gatewaytest

import (
	"slices"
	"strconv"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
)

const (
	kAdmissionID = "admission_id"
	kRowID       = "row_id"
	kCursor      = "cursor"
	kLimit       = "limit"
	kOrder       = "order"
	kCommand     = "command"
	reasonBusy   = "session_busy"
	reasonIdle   = "session_idle"
	roleUser     = "user"
	kPairs       = "pairs"
	kType        = "type"
	kOutput      = "output"
	statusStart  = "started"
)

// receipt is an admission receipt (gateway/session_contract.py
// AdmissionReceipt).
func receipt(sessionID, admission string, sequence int64, status string, generation any) map[string]any {
	return map[string]any{kAdmissionID: admission, "ref": map[string]any{kSessionID: sessionID}, "sequence": sequence,
		kStatus: status, kOutcome: nil, "authority_epoch": 1, kExecGen: generation}
}

// SteerEnded, as a SteerScript step, ends the running turn before the
// steer reaches it: the admission runs as its own turn and the steer is
// refused, as gateway/session_queued_controls.py does then.
const SteerEnded = "ended"

// interactiveSources are the sources whose queued prompts Hermes lets a
// client steer (gateway/session_queued_controls.py _INTERACTIVE_SOURCES).
func interactiveSources() []string { return []string{"cli", "tui", "gui", "desktop", "acp"} }

// SteerScript sets how the next prompt.steer_queued calls go: each step is a
// refusal reason, or SteerEnded; once the steps run out a steer succeeds.
func (g *Gateway) SteerScript(steps ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.steerScript = slices.Clone(steps)
}

// Steered lists the admissions prompt.steer_queued handed to a running
// turn.
func (g *Gateway) Steered() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.steered)
}

// Parent is the session a session was branched from, "" for none.
func (g *Gateway) Parent(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sessions[id]; s != nil {
		return s.Parent
	}
	return ""
}

// BranchWithoutParent makes the next branches record no parent, as a
// lineage the device plugin cannot read.
func (g *Gateway) BranchWithoutParent() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.orphanBranches = true
}

// queueLocked admits a prompt behind the running turn, as Hermes does: it
// joins the pending list and waits. g.mu is held.
func (g *Gateway) queueLocked(s *Session, submission, text string) map[string]any {
	admission := randHex()
	s.Pending = append(s.Pending, gateway.PendingAdmission{AdmissionID: admission, Sequence: int64(len(s.Pending) + 1),
		Status: gateway.AdmissionQueued, Text: text, InputID: submission})
	g.publishLocked(s, gateway.InfoEvent, map[string]any{kPending: s.Pending, kRevision: s.Revision, kExecGen: s.Generation})
	return receipt(s.ID, admission, int64(len(s.Pending)), gateway.AdmissionQueued, nil)
}

// FinishTurn ends a session's running turn and runs its queued prompts in
// order, each as a whole turn.
func (g *Gateway) FinishTurn(sessionID string) {
	g.mu.Lock()
	queued := g.finishLocked(g.sessions[sessionID])
	g.mu.Unlock()
	g.runQueued(sessionID, queued)
}

// finishLocked ends the running turn and takes the queued prompts, which
// the caller runs with runQueued. g.mu is held.
func (g *Gateway) finishLocked(s *Session) []gateway.PendingAdmission {
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "done", kStatus: kComplete})
	g.settleTransfersLocked(s, "steered")
	queued := s.Pending
	s.Pending = nil
	return queued
}

func (g *Gateway) runQueued(sessionID string, queued []gateway.PendingAdmission) {
	for _, a := range queued {
		g.mu.Lock()
		s := g.sessions[sessionID]
		s.Generation++
		s.Running = true
		s.Messages = append(s.Messages, map[string]any{kRole: roleUser, kText: a.Text, kSubmission: a.InputID})
		g.mu.Unlock()
		g.turn(sessionID, a.AdmissionID, a.InputID, a.Text)
	}
}

// composeHandler is the handler of a steer, transcript, info, command or
// background method, nil for another method.
func (g *Gateway) composeHandler(method string) func(*conn, map[string]any) (any, string, func()) {
	return map[string]func(*conn, map[string]any) (any, string, func()){
		gateway.MethodSteerQueued: g.hSteerQueued, gateway.MethodTranscript: g.hTranscript,
		gateway.MethodCommandsCatalog: g.hCatalog, gateway.MethodSlashExec: g.hSlashExec,
		gateway.MethodSessionInfo: g.hSessionInfo, gateway.MethodPromptBackground: g.hBackground,
	}[method]
}

// hSteerQueued follows gateway/session_queued_controls.py steer_queued.
func (g *Gateway) hSteerQueued(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, kSessionID, kProfile, kAdmissionID) || str(p, kAdmissionID) == "" {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, kNotFound, nil
	}
	i := slices.IndexFunc(s.Pending, func(a gateway.PendingAdmission) bool { return a.AdmissionID == str(p, kAdmissionID) })
	switch {
	case i < 0:
		return nil, gateway.ReasonAdmissionStarted, nil
	case !slices.Contains(interactiveSources(), sourceOf(s)):
		return nil, gateway.ReasonUnsupportedControl, nil
	case !s.Running:
		return nil, reasonIdle, nil
	}
	if len(g.steerScript) > 0 {
		step := g.steerScript[0]
		g.steerScript = g.steerScript[1:]
		if step != SteerEnded {
			return nil, step, nil
		}
		queued := g.finishLocked(s)
		return nil, gateway.ReasonSteerUnavailable, func() { g.runQueued(s.ID, queued) }
	}
	a := s.Pending[i]
	s.Pending[i].Status = statusStart
	s.transfers = append(s.transfers, a.AdmissionID)
	g.steered = append(g.steered, a.AdmissionID)
	g.publishLocked(s, gateway.InfoEvent, map[string]any{kPending: s.Pending, kRevision: s.Revision, kExecGen: s.Generation})
	return receipt(s.ID, a.AdmissionID, a.Sequence, statusStart, s.Generation), "", nil
}

// settleTransfersLocked ends the steers handed to the turn that ended,
// with outcome: "steered" when the turn finished, "cancelled" when it was
// stopped, as gateway/session_queued_controls.py publish_transfers does.
// g.mu is held.
func (g *Gateway) settleTransfersLocked(s *Session, outcome string) {
	if len(s.transfers) == 0 {
		return
	}
	for _, id := range s.transfers {
		s.Pending = slices.DeleteFunc(s.Pending, func(a gateway.PendingAdmission) bool { return a.AdmissionID == id })
	}
	g.publishLocked(s, gateway.InfoEvent, map[string]any{kPending: s.Pending, kRevision: s.Revision, kExecGen: s.Generation})
	for _, id := range s.transfers {
		g.publishAdmissionLocked(s, "message.complete", id, map[string]any{kText: "", "content": "", kAdmissionID: id, kOutcome: outcome})
	}
	s.transfers = nil
}

func sourceOf(s *Session) string {
	if s.Source == "" {
		return gateway.SourceACP
	}
	return s.Source
}

// rowID is a message's row id: the one the test set, else its place.
func rowID(m map[string]any, i int) int64 {
	if f, ok := m[kRowID].(float64); ok {
		return int64(f)
	}
	if n, ok := m[kRowID].(int64); ok {
		return n
	}
	if n, ok := m[kRowID].(int); ok {
		return int64(n)
	}
	return int64(i + 1)
}

func (s *Session) rowIndex(id int64) int {
	for i, m := range s.Messages {
		if rowID(m, i) == id {
			return i
		}
	}
	return -1
}

// rewind follows hermes_state_mutations.py _rewind.
func (g *Gateway) rewind(s *Session, payload, result map[string]any) string {
	target, ok := num(payload, "target_message_id")
	if len(payload) != 1 || !ok || target <= 0 {
		return errInvalidParams
	}
	if s.Running || len(s.Pending) > 0 {
		return reasonBusy
	}
	i := s.rowIndex(target)
	if i < 0 || s.Messages[i][kRole] != roleUser {
		return errInvalidParams
	}
	result["rewound_count"] = len(s.Messages) - i
	result["target_message"] = map[string]any{kRole: roleUser, "content": s.Messages[i][kText]}
	s.Messages = slices.Clone(s.Messages[:i])
	s.Generation++
	return ""
}

// branch follows hermes_state_mutation_branch.py: the new session records
// its parent and keeps its policy.
func (g *Gateway) branch(s *Session, payload, result map[string]any) string {
	if !onlyKeys(payload, kTitle, "through_message_id") {
		return errInvalidParams
	}
	if s.Running || len(s.Pending) > 0 {
		return reasonBusy
	}
	end := len(s.Messages)
	if _, set := payload["through_message_id"]; set {
		through, ok := num(payload, "through_message_id")
		i := s.rowIndex(through)
		if !ok || through <= 0 || i < 0 {
			return errInvalidParams
		}
		end = i + 1
	}
	child := &Session{ID: "branch-" + randHex()[:12], Title: str(payload, kTitle), Live: true, StartedAt: s.StartedAt,
		Messages: slices.Clone(s.Messages[:end]), Model: s.Model, Provider: s.Provider, CWD: s.CWD, RowCWD: s.RowCWD,
		Source: s.Source, Effort: s.Effort, Parent: s.ID, epoch: randHex(), prompts: map[string]gateway.Prompt{}, subs: map[*conn]string{}}
	if g.orphanBranches {
		child.Parent = ""
	}
	g.sessions[child.ID] = child
	result["branched_session_id"], result["copied_messages"] = child.ID, end
	return ""
}

// hTranscript follows gateway/session_stored_reads.py: display rows with
// row_kind, paged by an opaque cursor; an empty oldest-order page means
// caught up.
func (g *Gateway) hTranscript(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, kSessionID, kProfile, "subagent_id", kCursor, kLimit, kOrder, "inline_images") {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, kNotFound, nil
	}
	limit := int64(100)
	if n, ok := num(p, kLimit); ok {
		limit = n
	}
	order := str(p, kOrder)
	if order == "" {
		order = gateway.OrderOldest
	}
	if limit < 1 || limit > gateway.MaxTranscriptPage || (order != gateway.OrderOldest && order != gateway.OrderLatest) {
		return nil, errInvalidParams, nil
	}
	rows := make([]map[string]any, 0, len(s.Messages))
	for i, m := range s.Messages {
		r := map[string]any{"row_kind": "message", kRowID: rowID(m, i)}
		for k, v := range m {
			if k != kRowID {
				r[k] = v
			}
		}
		rows = append(rows, r)
	}
	cursor, set := p[kCursor].(string)
	at, err := strconv.Atoi(cursor)
	if set && err != nil {
		return nil, errInvalidParams, nil
	}
	n := int(limit)
	if order == gateway.OrderOldest {
		start := min(at, len(rows))
		end := min(start+n, len(rows))
		next := strconv.Itoa(end)
		return map[string]any{kSessionID: s.ID, "rows": rows[start:end], "next_cursor": next, "reset": !set}, "", nil
	}
	end := len(rows)
	if set {
		end = min(at, len(rows))
	}
	start := max(end-n, 0)
	var next any
	if start > 0 {
		next = strconv.Itoa(start)
	}
	return map[string]any{kSessionID: s.ID, "rows": rows[start:end], "next_cursor": next, "reset": !set}, "", nil
}

// pathCompress is /compress as the catalog names it.
const pathCompress = "/compress"

// Catalog is what the fake's commands.catalog lists: registry commands by
// category, a quick command and a skill.
func Catalog() map[string]any {
	return map[string]any{
		"categories": []map[string]any{
			{kName: "Session", kPairs: [][]string{{"/title", "Set the title (usage: /title [name])"},
				{"/model", "Switch model (usage: /model [name])"}, {"/usage", "Show token usage"}, {"/diff", "Show the git diff"},
				{pathCompress, "Compress context (usage: /compress [focus topic | --preview])"}}},
			{kName: "Tools", kPairs: [][]string{{"/plan", "Plan first (usage: /plan <task>)"}}},
			{kName: "User commands", kPairs: [][]string{{"/deploy", "exec: make deploy"}}},
		},
		kPairs:   [][]string{{"/title", "Set the title"}, {"/my-skill", "A skill"}},
		"skills": map[string]any{"/my-skill": map[string]any{"uses": 0, "origin": "local"}},
		"canon":  map[string]string{"/compact": pathCompress, pathCompress: pathCompress}, "commands": map[string]any{}, "sub": map[string]any{}, "skill_count": 1, "warning": "",
	}
}

func (g *Gateway) hCatalog(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, kProfile, kSessionID) {
		return nil, errInvalidParams, nil
	}
	return Catalog(), "", nil
}

// hSlashExec follows gateway/session_commands.py for a few commands:
// /usage reads, /plan becomes a prompt, /title renames, /export is refused
// with a hint and any other command is refused.
func (g *Gateway) hSlashExec(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, kSessionID, kProfile, kCommand) || str(p, kCommand) == "" {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, kNotFound, nil
	}
	g.commands = append(g.commands, str(p, kCommand))
	name, arg := splitCommand(str(p, kCommand))
	switch name {
	case "usage":
		return map[string]any{kType: gateway.SlashOutput, kOutput: "Tokens: 42"}, "", nil
	case "plan":
		return map[string]any{kType: gateway.SlashSend, kMessage: "Make a plan for: " + arg, "display": "/plan " + arg,
			"notice": "Planning first"}, "", nil
	case kTitle:
		s.Title = arg
		return map[string]any{kType: gateway.SlashOutput, kOutput: "Title set"}, "", nil
	case "export":
		return nil, gateway.ReasonUnsupportedCommand + "|Run `hermes profile export` in a terminal.", nil
	}
	return nil, gateway.ReasonUnsupportedCommand, nil
}

// Commands lists the command lines slash.exec got.
func (g *Gateway) Commands() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.commands)
}

func splitCommand(line string) (string, string) {
	for i, r := range line {
		if r == ' ' {
			return line[:i], line[i+1:]
		}
	}
	return line, ""
}
