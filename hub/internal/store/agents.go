package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.AgentStore = (*Store)(nil)

// agentMessageTTL is how long the store keeps agent messages.
const agentMessageTTL = 30 * 24 * time.Hour

const agentColumns = `id, backend_id, from_session, from_root, from_subagent, from_name, to_session, to_subagent,
	to_name, reply_to, hop, text, mode, status, created_at`

// AddAgentMessage records a message, and drops those older than 30 days.
func (s *Store) AddAgentMessage(ctx context.Context, m hub.AgentMessage) error {
	if err := s.exec(ctx, `DELETE FROM agent_messages WHERE created_at < ?`,
		s.now().Add(-agentMessageTTL).UnixMilli()); err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO agent_messages (`+agentColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.BackendID, m.FromSession, m.FromRoot, m.FromSubagent, m.FromName, m.ToSession, m.ToSubagent,
		m.ToName, m.ReplyTo, m.Hop, m.Text, m.Mode, m.Status, m.CreatedAt.UnixMilli())
}

// SetAgentMessageStatus records how a message was delivered.
func (s *Store) SetAgentMessageStatus(ctx context.Context, id, mode, status string) error {
	return s.exec(ctx, `UPDATE agent_messages SET mode = ?, status = ? WHERE id = ?`, mode, status, id)
}

func scanAgentMessage(r *sql.Row) (hub.AgentMessage, bool, error) {
	var m hub.AgentMessage
	var at int64
	err := r.Scan(&m.ID, &m.BackendID, &m.FromSession, &m.FromRoot, &m.FromSubagent, &m.FromName, &m.ToSession,
		&m.ToSubagent, &m.ToName, &m.ReplyTo, &m.Hop, &m.Text, &m.Mode, &m.Status, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.AgentMessage{}, false, nil
	}
	if err != nil {
		return hub.AgentMessage{}, false, err
	}
	m.CreatedAt = time.UnixMilli(at)
	return m, true, nil
}

// AgentMessage returns one message of the backend.
func (s *Store) AgentMessage(ctx context.Context, backendID, id string) (hub.AgentMessage, bool, error) {
	return scanAgentMessage(s.row(ctx, `SELECT `+agentColumns+` FROM agent_messages WHERE id = ? AND backend_id = ?`, id, backendID))
}

// MaxAgentHop is the highest hop of the messages any of sessions got
// since since, or 0.
func (s *Store) MaxAgentHop(ctx context.Context, backendID string, sessions []string, since time.Time) (int, error) {
	if len(sessions) == 0 {
		return 0, nil
	}
	args := []any{backendID, since.UnixMilli()}
	for _, sid := range sessions {
		args = append(args, sid)
	}
	var hop int
	err := s.row(ctx, `SELECT COALESCE(MAX(hop), 0) FROM agent_messages WHERE backend_id = ? AND created_at >= ?
		AND to_session IN (?`+strings.Repeat(", ?", len(sessions)-1)+`)`, args...).Scan(&hop)
	if err != nil {
		return 0, fmt.Errorf("agent message hops: %w", err)
	}
	return hop, nil
}

// AgentCounts counts the recent messages that limit a send.
func (s *Store) AgentCounts(ctx context.Context, backendID, fromRoot, toSession string, short, long time.Time) (hub.AgentCounts, error) {
	var c hub.AgentCounts
	err := s.row(ctx, `SELECT
		COALESCE(SUM(CASE WHEN from_root = ? AND created_at >= ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN from_root = ? AND to_session = ? AND created_at >= ? THEN 1 ELSE 0 END), 0),
		COUNT(*)
		FROM agent_messages WHERE backend_id = ? AND created_at >= ?`,
		fromRoot, short.UnixMilli(), fromRoot, toSession, short.UnixMilli(), backendID, long.UnixMilli()).
		Scan(&c.FromRoot, &c.Pair, &c.Backend)
	if err != nil {
		return hub.AgentCounts{}, fmt.Errorf("count agent messages: %w", err)
	}
	return c, nil
}

// AgentNames maps each named session of the backend to its name.
func (s *Store) AgentNames(ctx context.Context, backendID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT session_id, name FROM agent_names WHERE backend_id = ?`), backendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sid, name string
		if err := rows.Scan(&sid, &name); err != nil {
			return nil, err
		}
		out[sid] = name
	}
	return out, rows.Err()
}

// SetAgentName names a session; "" removes the name. A name another
// session of the backend has, in any case, is hub.ErrAgentNameTaken.
func (s *Store) SetAgentName(ctx context.Context, backendID, session, name string) error {
	if name == "" {
		return s.exec(ctx, `DELETE FROM agent_names WHERE backend_id = ? AND session_id = ?`, backendID, session)
	}
	key := strings.ToLower(name)
	var owner string
	err := s.row(ctx, `SELECT session_id FROM agent_names WHERE backend_id = ? AND name_key = ?`, backendID, key).Scan(&owner)
	switch {
	case err == nil && owner != session:
		return hub.ErrAgentNameTaken
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	err = s.exec(ctx, `INSERT INTO agent_names (backend_id, session_id, name, name_key, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (backend_id, session_id) DO UPDATE SET name = excluded.name, name_key = excluded.name_key,
		updated_at = excluded.updated_at`, backendID, session, name, key, s.now().UnixMilli())
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return hub.ErrAgentNameTaken
	}
	return err
}
