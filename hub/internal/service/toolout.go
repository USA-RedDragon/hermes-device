package service

import (
	"context"
	"encoding/json"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.ToolOutputs = (*Service)(nil)

// ToolOutput finds a tool row of the session in the snapshot the hub holds,
// and in the stored transcript when the snapshot has not got it yet. The
// user needs the same access as for the chat's events.
func (s *Service) ToolOutput(ctx context.Context, user, backendID, sessionID string, rowID int64, callID string) (json.RawMessage, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return nil, refusal(err)
	}
	ls.mu.Lock()
	snap := ls.snapshot.Data
	ls.mu.Unlock()
	ls.unpin()
	var m struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if len(snap) > 0 && json.Unmarshal(snap, &m) == nil {
		if out, ok := toolRow(m.Messages, rowID, callID); ok {
			return out, nil
		}
	}
	t, err := s.Transcript(ctx, user, backendID, sessionID)
	if err != nil {
		return nil, err
	}
	if out, ok := toolRow(t.Messages, rowID, callID); ok {
		return out, nil
	}
	return nil, hub.ErrNoToolOutput
}

// toolRow picks the output fields of the tool row that rowID or callID
// names.
func toolRow(rows []json.RawMessage, rowID int64, callID string) (json.RawMessage, bool) {
	for _, raw := range rows {
		var r struct {
			Role       string          `json:"role"`
			RowID      *int64          `json:"row_id"`
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
			Text       json.RawMessage `json:"text"`
		}
		if json.Unmarshal(raw, &r) != nil || r.Role != "tool" {
			continue
		}
		if rowID > 0 {
			if r.RowID == nil || *r.RowID != rowID {
				continue
			}
		} else if callID == "" || r.ToolCallID != callID {
			continue
		}
		out := map[string]json.RawMessage{}
		if len(r.Content) > 0 {
			out["content"] = r.Content
		}
		if len(r.Text) > 0 {
			out["text"] = r.Text
		}
		b, err := json.Marshal(out)
		if err != nil {
			return nil, false
		}
		return b, true
	}
	return nil, false
}
