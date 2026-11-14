package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.AgentStore = (*Store)(nil)

// agentMessageTTL is how long the store keeps agent messages.
const agentMessageTTL = 30 * 24 * time.Hour

const agentColumns = `id, backend_id, from_session, from_root, from_subagent, from_name, to_session, to_subagent,
	to_name, reply_to, hop, text, mode, status, in_tree, created_at`

// AddAgentMessage records a message, and drops those older than 30 days.
func (s *Store) AddAgentMessage(ctx context.Context, m hub.AgentMessage) error {
	inTree := 0
	if m.InTree {
		inTree = 1
	}
	if err := s.exec(ctx, `DELETE FROM agent_messages WHERE created_at < ?`,
		s.now().Add(-agentMessageTTL).UnixMilli()); err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO agent_messages (`+agentColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.BackendID, m.FromSession, m.FromRoot, m.FromSubagent, m.FromName, m.ToSession, m.ToSubagent,
		m.ToName, m.ReplyTo, m.Hop, m.Text, m.Mode, m.Status, inTree, m.CreatedAt.UnixMilli())
}

// SetAgentMessageStatus records how a message was delivered.
func (s *Store) SetAgentMessageStatus(ctx context.Context, id, mode, status string) error {
	return s.exec(ctx, `UPDATE agent_messages SET mode = ?, status = ? WHERE id = ?`, mode, status, id)
}

func scanAgentMessage(r *sql.Row) (hub.AgentMessage, bool, error) {
	var m hub.AgentMessage
	var at, inTree int64
	err := r.Scan(&m.ID, &m.BackendID, &m.FromSession, &m.FromRoot, &m.FromSubagent, &m.FromName, &m.ToSession,
		&m.ToSubagent, &m.ToName, &m.ReplyTo, &m.Hop, &m.Text, &m.Mode, &m.Status, &inTree, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.AgentMessage{}, false, nil
	}
	if err != nil {
		return hub.AgentMessage{}, false, err
	}
	m.InTree = inTree != 0
	m.CreatedAt = time.UnixMilli(at)
	return m, true, nil
}

// AgentMessage returns one message of the backend.
func (s *Store) AgentMessage(ctx context.Context, backendID, id string) (hub.AgentMessage, bool, error) {
	return scanAgentMessage(s.row(ctx, `SELECT `+agentColumns+` FROM agent_messages WHERE id = ? AND backend_id = ?`, id, backendID))
}

// LastAgentIn is the newest message at or after since that any of
// toSessions got from the session or tree from.
func (s *Store) LastAgentIn(ctx context.Context, backendID string, toSessions []string, from string, since time.Time) (hub.AgentMessage, bool, error) {
	if len(toSessions) == 0 {
		return hub.AgentMessage{}, false, nil
	}
	args := []any{backendID, since.UnixMilli(), from, from}
	for _, sid := range toSessions {
		args = append(args, sid)
	}
	return scanAgentMessage(s.row(ctx, `SELECT `+agentColumns+` FROM agent_messages WHERE backend_id = ? AND created_at >= ?
		AND (from_root = ? OR from_session = ?) AND to_session IN (?`+strings.Repeat(", ?", len(toSessions)-1)+`)
		ORDER BY created_at DESC, hop DESC LIMIT 1`, args...))
}

// LastAgentOut is the newest message at or after since that the tree
// of fromRoot sent to toSession.
func (s *Store) LastAgentOut(ctx context.Context, backendID, fromRoot, toSession string, since time.Time) (hub.AgentMessage, bool, error) {
	return scanAgentMessage(s.row(ctx, `SELECT `+agentColumns+` FROM agent_messages WHERE backend_id = ? AND created_at >= ?
		AND from_root = ? AND to_session = ? ORDER BY created_at DESC, hop DESC LIMIT 1`,
		backendID, since.UnixMilli(), fromRoot, toSession))
}

// AgentCounts counts the recent messages that limit a send. Messages
// inside one delegation tree are not counted.
func (s *Store) AgentCounts(ctx context.Context, backendID, fromRoot, toSession string, short, long time.Time) (hub.AgentCounts, error) {
	var c hub.AgentCounts
	err := s.row(ctx, `SELECT
		COALESCE(SUM(CASE WHEN from_root = ? AND created_at >= ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN from_root = ? AND to_session = ? AND created_at >= ? THEN 1 ELSE 0 END), 0),
		COUNT(*)
		FROM agent_messages WHERE backend_id = ? AND created_at >= ? AND in_tree = 0`,
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

// AgentPartners lists the user's partner pairs on a backend.
func (s *Store) AgentPartners(ctx context.Context, user, backendID string) ([]hub.AgentPartner, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT session_a, session_b FROM agent_partners WHERE "user" = ? AND backend_id = ?
		ORDER BY created_at, session_a, session_b`), user, backendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hub.AgentPartner{}
	for rows.Next() {
		var p hub.AgentPartner
		if err := rows.Scan(&p.A, &p.B); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// agentPartnerLockKey is the first key of the PostgreSQL advisory lock
// that keeps two adds of one user from passing the limit together.
const agentPartnerLockKey = 0x61706172 // "apar"

// AddAgentPartner adds a pair; an existing pair stays.
func (s *Store) AddAgentPartner(ctx context.Context, user, backendID string, p hub.AgentPartner) error {
	p = hub.NewAgentPartner(p.A, p.B)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if s.dialect == postgresDialect {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, agentPartnerLockKey, user); err != nil {
				return err
			}
		}
		var have int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM agent_partners WHERE "user" = ? AND backend_id = ?
			AND session_a = ? AND session_b = ?`), user, backendID, p.A, p.B).Scan(&have); err != nil {
			return err
		}
		if have > 0 {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM agent_partners WHERE "user" = ? AND backend_id = ?`), user, backendID).Scan(&n); err != nil {
			return err
		}
		if n >= hub.MaxAgentPartners {
			return hub.ErrTooManyAgentPartners
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO agent_partners ("user", backend_id, session_a, session_b, created_at)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`), user, backendID, p.A, p.B, s.now().UnixMilli())
		return err
	})
}

// RemoveAgentPartner removes a pair and reports whether it was there.
func (s *Store) RemoveAgentPartner(ctx context.Context, user, backendID string, p hub.AgentPartner) (bool, error) {
	p = hub.NewAgentPartner(p.A, p.B)
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM agent_partners WHERE "user" = ? AND backend_id = ? AND session_a = ? AND session_b = ?`),
		user, backendID, p.A, p.B)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
