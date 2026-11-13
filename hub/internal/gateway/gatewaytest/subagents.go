package gatewaytest

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
)

const (
	kNextCursor = "next_cursor"
	kAvailable  = "available"
	kReset      = "reset"
	kRows       = "rows"
)

// child is the stored transcript of one delegated child, as
// session.transcript with subagent_id reads it.
type child struct {
	rows []map[string]any
	// gen moves when the transcript is rewritten: a cursor of another
	// generation reads it again from the start, with reset.
	gen int
}

// AddChildRows appends rows to the transcript of subagent subagentID of
// the session. Rows are display rows as a Hermes with tool_details sends
// them (role, text, timestamp, tool_calls, tool_call_id, name, args,
// result, result_size, display_metadata); the fake adds row_kind and
// row_id. A subagent that subagent.list does not list reads as ended.
func (g *Gateway) AddChildRows(sessionID, subagentID string, rows ...map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	if s.children == nil {
		s.children = map[string]*child{}
	}
	c := s.children[subagentID]
	if c == nil {
		c = &child{}
		s.children[subagentID] = c
	}
	c.rows = append(c.rows, rows...)
}

// RewriteChild replaces a child's transcript, as a compaction does: the
// next read from an older cursor starts again with reset.
func (g *Gateway) RewriteChild(sessionID, subagentID string, rows ...map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.sessions[sessionID].children[subagentID]
	c.rows = slices.Clone(rows)
	c.gen++
}

// EndSubagent drops a subagent from subagent.list and publishes its
// subagent.complete with status.
func (g *Gateway) EndSubagent(sessionID, subagentID, status, summary string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	if i := s.subagent(subagentID); i >= 0 {
		s.Subagents = slices.Delete(s.Subagents, i, i+1)
	}
	g.publishLocked(s, "subagent.complete", map[string]any{kSubagentID: subagentID, kStatus: status, "summary": summary,
		"task_count": 1, "task_index": 0, "input_tokens": 3100, "output_tokens": 920, "duration_seconds": 1300.0})
}

// childTranscript serves session.transcript with subagent_id
// (gateway/session_transcript_subagent.py): the child's rows paged by a
// cursor of its generation; a child the session never had is
// available: false.
func (g *Gateway) childTranscript(s *Session, p map[string]any, limit int) (any, string, func()) {
	id := str(p, kSubagentID)
	details, hasDetails := num(p, "tool_details")
	if hasDetails && (!slices.Contains(g.caps, gateway.CapTranscriptToolDetails) || details < 0 || details > gateway.MaxToolDetails) {
		return nil, errInvalidParams, nil
	}
	running := s.subagent(id) >= 0
	c := s.children[id]
	if c == nil {
		return map[string]any{kSessionID: s.ID, kRows: []any{}, kNextCursor: nil, kReset: true, kSubagentID: id,
			kAvailable: false, statusRun: running, "child_session_id": nil}, "", nil
	}
	start, reset := 0, true
	if cursor, ok := p[kCursor].(string); ok {
		gen, at, found := strings.Cut(cursor, ":")
		n, err := strconv.Atoi(at)
		if !found || err != nil {
			return nil, errInvalidParams, nil
		}
		if gen == strconv.Itoa(c.gen) {
			start, reset = min(n, len(c.rows)), false
		}
	}
	var rows []map[string]any
	end := start
	for end < len(c.rows) && len(rows) < limit {
		if r := childRow(c.rows[end], end, details > 0); r != nil {
			rows = append(rows, r)
		}
		end++
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	return map[string]any{kSessionID: s.ID, kRows: rows, kNextCursor: fmt.Sprintf("%d:%d", c.gen, end), kReset: reset,
		kSubagentID: id, kAvailable: true, statusRun: running, "child_session_id": "child-" + id}, "", nil
}

// childRow is row i as Hermes projects it; without tool details an
// assistant row with no text is dropped, and calls and results are not
// sent.
func childRow(m map[string]any, i int, details bool) map[string]any {
	r := map[string]any{"row_kind": "message", kRowID: i + 1}
	for k, v := range m {
		r[k] = v
	}
	if !details {
		delete(r, "tool_calls")
		delete(r, "result")
		delete(r, "result_size")
		if text, _ := r["text"].(string); r["role"] == "assistant" && strings.TrimSpace(text) == "" {
			return nil
		}
	}
	return r
}
