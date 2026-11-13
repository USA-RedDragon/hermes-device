package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.SubagentTranscripts = (*Service)(nil)

const (
	// subagentPage is the rows one session.transcript page of a subagent
	// asks for, subagentPages the pages one request reads at most.
	subagentPage  = 200
	subagentPages = 10
	// subagentEventsMax is the most events one answer has.
	subagentEventsMax = 500
	// subagentLogKeep is how long the hub keeps a log no read wrote to.
	subagentLogKeep = 90 * 24 * time.Hour
)

// subagentLocks keeps two reads of one subagent from reading its transcript
// at once in this replica; the store's cursor check covers the others.
type subagentLocks struct {
	mu     sync.Mutex
	locks  map[hub.SubagentKey]*sync.Mutex
	pruned time.Time
}

func (l *subagentLocks) lock(k hub.SubagentKey) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[hub.SubagentKey]*sync.Mutex{}
	}
	m, ok := l.locks[k]
	if !ok {
		m = &sync.Mutex{}
		l.locks[k] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// prune reports whether the hourly prune of old logs is due.
func (l *subagentLocks) prune(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.pruned) < time.Hour {
		return false
	}
	l.pruned = now
	return true
}

// subagentLiveWait bounds the live read of Hermes inside SubagentEvents, so a
// Hermes that is restarting or gone leaves the stored copy to answer fast.
const subagentLiveWait = 4 * time.Second

// SubagentEvents reads the steps of one subagent of the session after seq
// after. It reads what Hermes wrote since the last read into the hub's log
// first, so the steps stay after the subagent ends and Hermes forgets it;
// when Hermes cannot answer, the log alone answers. A user reads only the
// log of a chat on a backend they may use, under their own name.
func (s *Service) SubagentEvents(ctx context.Context, user, backendID, sessionID, subagentID string, after int64) (hub.SubagentEvents, error) {
	out := hub.SubagentEvents{Events: []hub.SubagentEvent{}}
	if subagentID == "" || sessionID == "" {
		return out, hub.ErrUnknownSubagent
	}
	if _, err := s.backend(ctx, user, backendID); err != nil {
		return out, err
	}
	log := s.opts.SubagentLog
	k := hub.SubagentKey{User: user, BackendID: backendID, SessionID: sessionID, SubagentID: subagentID}
	var liveErr error
	if log != nil {
		unlock := s.subagentLocks.lock(k)
		lctx, cancel := context.WithTimeout(ctx, subagentLiveWait)
		liveErr = s.withSession(lctx, user, backendID, sessionID, func(b *backend, ls *live) error {
			return s.syncSubagent(lctx, b, ls, k, &out)
		})
		cancel()
		unlock()
		if errors.Is(liveErr, hub.ErrForbidden) || errors.Is(liveErr, hub.ErrUnknownBackend) {
			return out, liveErr
		}
		if liveErr != nil {
			s.log.Debug("subagent transcript", "error", liveErr.Error())
		}
		if now := time.Now(); s.subagentLocks.prune(now) {
			if err := log.PruneSubagentLogs(ctx, now.Add(-subagentLogKeep)); err != nil {
				s.log.Debug("prune subagent logs", "error", err.Error())
			}
		}
		_, epoch, err := log.SubagentCursor(ctx, k)
		if err != nil {
			return out, err
		}
		out.Epoch = epoch
		evs, err := log.SubagentEvents(ctx, k, after, subagentEventsMax+1)
		if err != nil {
			return out, err
		}
		if len(evs) > subagentEventsMax {
			evs, out.More = evs[:subagentEventsMax], true
		}
		out.Events = evs
		if liveErr == nil {
			out.Live = true
		}
		return out, nil
	}
	// Without a log the hub reads the whole transcript every time.
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		var all []hub.SubagentEvent
		_, _, err := s.readChild(ctx, b, ls, subagentID, "", true, func(evs []hub.SubagentEvent, _ bool) { all = append(all, evs...) }, &out)
		for i := range all {
			all[i].Seq = int64(i + 1)
		}
		for _, e := range all {
			if e.Seq > after && len(out.Events) < subagentEventsMax {
				out.Events = append(out.Events, e)
			}
		}
		return err
	})
	if err == nil {
		out.Live = true
	}
	return out, err
}

// syncSubagent reads what the subagent's transcript gained since the log's
// cursor into the log.
func (s *Service) syncSubagent(ctx context.Context, b *backend, ls *live, k hub.SubagentKey, out *hub.SubagentEvents) error {
	log := s.opts.SubagentLog
	from, _, err := log.SubagentCursor(ctx, k)
	if err != nil {
		return err
	}
	var pending []hub.SubagentEvent
	reset := false
	to, found, err := s.readChild(ctx, b, ls, k.SubagentID, from, false, func(evs []hub.SubagentEvent, r bool) {
		if r && from != "" {
			pending, reset = nil, true
		}
		pending = append(pending, evs...)
	}, out)
	if err != nil || !found || to == from && len(pending) == 0 && !reset {
		return err
	}
	_, err = log.AppendSubagentEvents(ctx, k, from, to, reset, pending)
	return err
}

// readChild reads the child's transcript pages from cursor on and hands each
// page's events to take, with whether the page starts the transcript again.
// It returns the cursor to read on from and whether Hermes found the child.
//
// With retry the read waits out a restarting backend (backend.read); without
// it one failed try ends the read, for a caller that has the log to answer.
func (s *Service) readChild(ctx context.Context, b *backend, ls *live, subagentID, cursor string, retry bool,
	take func([]hub.SubagentEvent, bool), out *hub.SubagentEvents) (string, bool, error) {
	c, err := b.ensure(ctx, ls)
	if err != nil {
		return cursor, false, refusal(err)
	}
	cctx, cancel := b.timeout(ctx)
	details := b.capable(cctx, c, gateway.CapTranscriptToolDetails)
	cancel()
	out.Details = details
	conv := newRowConverter()
	// A read that goes on from a cursor is past the goal.
	conv.goal = cursor != ""
	for range subagentPages {
		p := gateway.TranscriptParams{SessionID: ls.id, SubagentID: subagentID, Cursor: cursor, Limit: subagentPage,
			Order: gateway.OrderOldest}
		if details {
			p.ToolDetails = hub.MaxStepResult
		}
		var r gateway.ChildTranscript
		call := b.call
		if retry {
			call = b.read
		}
		if err := call(ctx, ls, gateway.MethodTranscript, p, &r); err != nil {
			return cursor, false, refusal(err)
		}
		if !r.Available {
			out.Running = r.Running
			return cursor, false, nil
		}
		out.Running = r.Running
		if r.Reset {
			conv = newRowConverter()
		}
		take(conv.rows(r.Rows), r.Reset)
		if r.NextCursor != nil {
			cursor = *r.NextCursor
		}
		if len(r.Rows) == 0 || r.NextCursor == nil {
			break
		}
	}
	return cursor, true, nil
}

// Roles of transcript rows.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// rowConverter turns transcript rows into steps. It remembers the calls it
// saw so a result names its call.
type rowConverter struct {
	calls map[string]string
	goal  bool
}

func newRowConverter() *rowConverter { return &rowConverter{calls: map[string]string{}} }

func (c *rowConverter) rows(raw []json.RawMessage) []hub.SubagentEvent {
	var out []hub.SubagentEvent
	for _, r := range raw {
		var row gateway.TranscriptRow
		if json.Unmarshal(r, &row) != nil {
			continue
		}
		out = append(out, c.row(row)...)
	}
	return out
}

func (c *rowConverter) row(row gateway.TranscriptRow) []hub.SubagentEvent {
	// A row Hermes gave no time stays without one: stamping it with the time
	// of this read would show a column of steps all at the moment of the read.
	var at time.Time
	if row.Timestamp != nil && *row.Timestamp > 0 {
		at = time.UnixMilli(int64(*row.Timestamp * 1000)).UTC()
	}
	if row.RowKind == "compaction" {
		return []hub.SubagentEvent{{TS: at, Type: hub.StepCompaction, Text: capText(row.Text)}}
	}
	switch row.Role {
	case roleUser:
		kind := hub.StepUser
		if !c.goal {
			kind, c.goal = hub.StepGoal, true
		}
		return []hub.SubagentEvent{{TS: at, Type: kind, Text: capText(row.Text)}}
	case roleAssistant:
		var out []hub.SubagentEvent
		if t := rawText(row.Reasoning); t != "" {
			out = append(out, hub.SubagentEvent{TS: at, Type: hub.StepThinking, Text: capText(t)})
		} else if t := rawText(row.ReasoningContent); t != "" {
			out = append(out, hub.SubagentEvent{TS: at, Type: hub.StepThinking, Text: capText(t)})
		}
		if strings.TrimSpace(row.Text) != "" {
			out = append(out, hub.SubagentEvent{TS: at, Type: hub.StepMessage, Text: capText(row.Text)})
		}
		for _, tc := range row.ToolCalls {
			c.calls[tc.ID] = tc.Name
			out = append(out, hub.SubagentEvent{TS: at, Type: hub.StepToolStart, CallID: tc.ID, Name: tc.Name, Args: compactArgs(tc.Args)})
		}
		return out
	case roleTool:
		name := row.Name
		if name == "" {
			name = c.calls[row.ToolCallID]
		}
		e := hub.SubagentEvent{TS: at, Type: hub.StepToolEnd, CallID: row.ToolCallID, Name: name, Args: compactArgs(row.Args)}
		if row.Result != nil {
			e.Result, e.ResultSize = capText(*row.Result), row.ResultSize
			e.Error = resultFailed(*row.Result)
		}
		var md struct {
			ToolOutcome *gateway.ToolOutcome `json:"tool_outcome"`
		}
		if len(row.DisplayMetadata) > 0 && json.Unmarshal(row.DisplayMetadata, &md) == nil && md.ToolOutcome != nil {
			// A duration of 0 is no measure: leave it unset so none is shown.
			if d := md.ToolOutcome.DurationMS; d > 0 {
				e.DurationMS = &d
			}
			e.Error = md.ToolOutcome.IsError
		}
		return []hub.SubagentEvent{e}
	}
	return nil
}

// rawText is a JSON string's text, or "".
func rawText(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// capText cuts s to hub.MaxStepResult characters.
func capText(s string) string {
	if utf8.RuneCountInString(s) <= hub.MaxStepResult {
		return s
	}
	r := []rune(s)
	return string(r[:hub.MaxStepResult])
}

// compactArgs keeps a call's arguments when they are a JSON object.
func compactArgs(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// resultFailed reads a failure from a whole JSON result: an error text, or
// a command's nonzero exit code. A cut result reads as no failure.
func resultFailed(s string) bool {
	var r struct {
		Error    any  `json:"error"`
		ExitCode *int `json:"exit_code"`
	}
	if json.Unmarshal([]byte(s), &r) != nil {
		return false
	}
	if e, ok := r.Error.(string); ok && e != "" {
		return true
	}
	return r.ExitCode != nil && *r.ExitCode != 0
}

// recordSteer keeps a message the user sent a subagent in its log.
func (s *Service) recordSteer(ctx context.Context, user, backendID, sessionID, subagentID, text string) {
	if s.opts.SubagentLog == nil {
		return
	}
	k := hub.SubagentKey{User: user, BackendID: backendID, SessionID: sessionID, SubagentID: subagentID}
	if _, err := s.opts.SubagentLog.AddSubagentSteer(ctx, k, text, time.Now()); err != nil {
		s.log.Warn("store subagent message", "error", err.Error())
	}
}
