package gatewaytest

import (
	"slices"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
)

const (
	kSubagentID  = "subagent_id"
	kProfile     = "profile"
	kProcessID   = "process_id"
	kPaused      = "paused"
	kTaskID      = "task_id"
	statusRun    = "running"
	statusKill   = "killed"
	kTruncated   = "truncated"
	kInterrupted = "interrupted"
	kNotFound    = "not_found"
)

// Steer is one message the fake queued for a subagent.
type Steer struct {
	SessionID, SubagentID, Text string
}

// SetPaused sets the gateway's switch for new subagents.
func (g *Gateway) SetPaused(paused bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused = paused
}

// Steers lists the messages subagent.steer queued.
func (g *Gateway) Steers() []Steer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.steers)
}

// sessionLocked is the session a session-scoped activity call names, as
// gateway/session_ancillary.py checks it: only the listed keys, a
// non-empty session_id of a live session.
func (g *Gateway) sessionLocked(p map[string]any, allowed ...string) (*Session, string) {
	if !onlyKeys(p, append(allowed, kSessionID, kProfile)...) || str(p, kSessionID) == "" {
		return nil, errInvalidParams
	}
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, kNotFound
	}
	return s, ""
}

func (g *Gateway) hSubagentList(_ *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p)
	if rerr != "" {
		return nil, rerr, nil
	}
	rows := s.Subagents
	if rows == nil {
		rows = []gateway.SubagentRow{}
	}
	return map[string]any{"subagents": rows, "delegations": []any{}}, "", nil
}

func (s *Session) subagent(id string) int {
	return slices.IndexFunc(s.Subagents, func(r gateway.SubagentRow) bool { return r.SubagentID == id })
}

func (g *Gateway) hSubagentTail(_ *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p, kSubagentID)
	if rerr != "" {
		return nil, rerr, nil
	}
	id := str(p, kSubagentID)
	if s.subagent(id) < 0 {
		return map[string]any{kSubagentID: id, "available": false, kText: "", kTruncated: false}, "", nil
	}
	return map[string]any{kSubagentID: id, "available": true, kText: "=== transcript ===\n" + id + " working", kTruncated: false}, "", nil
}

func (g *Gateway) hSubagentSteer(_ *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p, kSubagentID, kText)
	if rerr != "" {
		return nil, rerr, nil
	}
	id, text := str(p, kSubagentID), str(p, kText)
	if id == "" || text == "" {
		return nil, errInvalidParams, nil
	}
	i := s.subagent(id)
	if i < 0 || s.Subagents[i].AcceptingSteer == nil || !*s.Subagents[i].AcceptingSteer {
		return map[string]any{kStatus: "rejected", kSubagentID: id, kText: text}, "", nil
	}
	g.steers = append(g.steers, Steer{SessionID: s.ID, SubagentID: id, Text: text})
	return map[string]any{kStatus: "queued", kSubagentID: id, kText: text}, "", nil
}

func (g *Gateway) hSubagentInterrupt(_ *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p, kSubagentID)
	if rerr != "" {
		return nil, rerr, nil
	}
	id := str(p, kSubagentID)
	i := s.subagent(id)
	if i < 0 {
		return map[string]any{"found": false, kSubagentID: id}, "", nil
	}
	row := s.Subagents[i]
	s.Subagents = slices.Delete(s.Subagents, i, i+1)
	g.publishLocked(s, "subagent.complete", map[string]any{"goal": row.Goal, "task_count": 1, "task_index": 0,
		kSubagentID: id, kStatus: kInterrupted, "input_tokens": 10, "output_tokens": 2})
	return map[string]any{"found": true, kSubagentID: id}, "", nil
}

func (g *Gateway) hProcessList(_ *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p)
	if rerr != "" {
		return nil, rerr, nil
	}
	rows := s.Processes
	if rows == nil {
		rows = []gateway.ProcessRow{}
	}
	return map[string]any{"processes": rows}, "", nil
}

func (g *Gateway) hProcessKill(_ *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p, kProcessID)
	if rerr != "" {
		return nil, rerr, nil
	}
	i := slices.IndexFunc(s.Processes, func(r gateway.ProcessRow) bool { return r.SessionID == str(p, kProcessID) })
	if i < 0 {
		return nil, "process_not_found", nil
	}
	reason := statusKill
	s.Processes[i].Status, s.Processes[i].CompletionReason = "exited", &reason
	return map[string]any{kStatus: statusKill, kSessionID: s.Processes[i].SessionID}, "", nil
}

// hDelegationStatus answers for every session of the gateway, as
// tui_gateway/methods_session.py does.
func (g *Gateway) hDelegationStatus(_ *conn, _ map[string]any) (any, string, func()) {
	active := []gateway.SubagentRow{}
	for _, s := range g.sessions {
		active = append(active, s.Subagents...)
	}
	return map[string]any{"active": active, kPaused: g.paused, "max_spawn_depth": 1, "max_concurrent_children": 3}, "", nil
}

func (g *Gateway) hDelegationPause(_ *conn, p map[string]any) (any, string, func()) {
	paused, ok := p[kPaused].(bool)
	if !ok || !onlyKeys(p, kPaused, kSessionID, kProfile) {
		return nil, errInvalidParams, nil
	}
	if id := str(p, kSessionID); id != "" && g.sessions[id] == nil {
		return nil, kNotFound, nil
	}
	g.paused = paused
	return map[string]any{kPaused: paused}, "", nil
}

// hBackground starts a /bg prompt that answers at once with its text.
func (g *Gateway) hBackground(c *conn, p map[string]any) (any, string, func()) {
	s, rerr := g.sessionLocked(p, kText)
	if rerr != "" {
		return nil, rerr, nil
	}
	if _, ok := s.subs[c]; !ok {
		return nil, errDenied, nil
	}
	text := str(p, kText)
	if text == "" {
		return nil, errInvalidParams, nil
	}
	id := "bg_" + randHex()[:6]
	return map[string]any{kTaskID: id}, "", func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.publishLocked(s, gateway.BackgroundCompleteEvent, map[string]any{kTaskID: id, kText: "done: " + text})
	}
}

// hRoster counts the running subagents and processes of every session.
func (g *Gateway) hRoster(_ *conn, _ map[string]any) (any, string, func()) {
	rows := []map[string]any{}
	for _, s := range g.sessions {
		bg := 0
		for _, r := range s.Processes {
			if r.Status == statusRun {
				bg++
			}
		}
		if len(s.Subagents) > 0 || bg > 0 {
			rows = append(rows, map[string]any{kSessionID: s.ID, "subagents": len(s.Subagents), "background": bg})
		}
	}
	return map[string]any{"sessions": rows}, "", nil
}

// hSessionInfo answers a session's info without subscribing, and
// not_found for a session the fake does not have
// (gateway/session_controls.py info).
func (g *Gateway) hSessionInfo(_ *conn, p map[string]any) (any, string, func()) {
	if len(p) != 1 || str(p, kSessionID) == "" {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, kNotFound, nil
	}
	return g.infoLocked(s, false), "", nil
}
