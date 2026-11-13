package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.SubagentLog = (*Store)(nil)

const subagentKeyWhere = `"user" = ? AND backend_id = ? AND session_id = ? AND subagent_id = ?`

func keyArgs(k hub.SubagentKey) []any {
	return []any{k.User, k.BackendID, k.SessionID, k.SubagentID}
}

// SubagentCursor is where the last read of the subagent's transcript ended.
func (s *Store) SubagentCursor(ctx context.Context, k hub.SubagentKey) (string, int64, error) {
	var cursor string
	var epoch int64
	err := s.row(ctx, `SELECT read_cursor, epoch FROM subagent_logs WHERE `+subagentKeyWhere, keyArgs(k)...).Scan(&cursor, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return cursor, epoch, err
}

// ensureLog makes the key's log row and returns its cursor, epoch and next
// seq, locking the row for the transaction.
func (s *Store) ensureLog(ctx context.Context, tx *sql.Tx, k hub.SubagentKey) (cursor string, epoch, next int64, err error) {
	now := s.now().Unix()
	if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO subagent_logs ("user", backend_id, session_id, subagent_id, read_cursor, epoch, next_seq, updated_at)
		VALUES (?, ?, ?, ?, '', 0, 1, ?) ON CONFLICT DO NOTHING`), append(keyArgs(k), now)...); err != nil {
		return "", 0, 0, err
	}
	// The update takes the row's lock in PostgreSQL, so two writers of one
	// log take turns; SQLite has one writer anyway.
	if _, err = tx.ExecContext(ctx, s.q(`UPDATE subagent_logs SET updated_at = ? WHERE `+subagentKeyWhere), append([]any{now}, keyArgs(k)...)...); err != nil {
		return "", 0, 0, err
	}
	err = tx.QueryRowContext(ctx, s.q(`SELECT read_cursor, epoch, next_seq FROM subagent_logs WHERE `+subagentKeyWhere), keyArgs(k)...).
		Scan(&cursor, &epoch, &next)
	return cursor, epoch, next, err
}

func (s *Store) insertEvents(ctx context.Context, tx *sql.Tx, k hub.SubagentKey, next int64, evs []hub.SubagentEvent) (int64, error) {
	for i := range evs {
		e := &evs[i]
		e.Seq = next
		next++
		var dur sql.NullInt64
		if e.DurationMS != nil {
			dur = sql.NullInt64{Int64: *e.DurationMS, Valid: true}
		}
		failed := 0
		if e.Error {
			failed = 1
		}
		args := ""
		if len(e.Args) > 0 {
			args = string(e.Args)
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO subagent_events ("user", backend_id, session_id, subagent_id, seq, at_ms, kind,
			call_id, name, args, result, result_size, duration_ms, failed, text) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			append(keyArgs(k), e.Seq, e.TS.UnixMilli(), e.Type, e.CallID, e.Name, args, e.Result, e.ResultSize, dur, failed, e.Text)...); err != nil {
			return 0, fmt.Errorf("subagent event: %w", err)
		}
	}
	return next, nil
}

// AppendSubagentEvents adds evs after the key's events and moves its cursor
// on, when it still is from.
func (s *Store) AppendSubagentEvents(ctx context.Context, k hub.SubagentKey, from, to string, reset bool, evs []hub.SubagentEvent) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	cursor, epoch, next, err := s.ensureLog(ctx, tx, k)
	if err != nil {
		return false, err
	}
	if cursor != from {
		return false, nil
	}
	if reset {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM subagent_events WHERE `+subagentKeyWhere+` AND kind <> ?`),
			append(keyArgs(k), hub.StepSteer)...); err != nil {
			return false, err
		}
		epoch++
	}
	next, err = s.insertEvents(ctx, tx, k, next, evs)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.q(`UPDATE subagent_logs SET read_cursor = ?, epoch = ?, next_seq = ? WHERE `+subagentKeyWhere),
		append([]any{to, epoch, next}, keyArgs(k)...)...); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SubagentEvents lists up to limit events of the key after seq after.
func (s *Store) SubagentEvents(ctx context.Context, k hub.SubagentKey, after int64, limit int) ([]hub.SubagentEvent, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT seq, at_ms, kind, call_id, name, args, result, result_size, duration_ms, failed, text
		FROM subagent_events WHERE `+subagentKeyWhere+` AND seq > ? ORDER BY seq LIMIT ?`), append(keyArgs(k), after, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []hub.SubagentEvent{}
	for rows.Next() {
		var e hub.SubagentEvent
		var at int64
		var args string
		var dur sql.NullInt64
		var failed int
		if err := rows.Scan(&e.Seq, &at, &e.Type, &e.CallID, &e.Name, &args, &e.Result, &e.ResultSize, &dur, &failed, &e.Text); err != nil {
			return nil, err
		}
		e.TS = time.UnixMilli(at).UTC()
		if args != "" {
			e.Args = json.RawMessage(args)
		}
		if dur.Valid {
			d := dur.Int64
			e.DurationMS = &d
		}
		e.Error = failed != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddSubagentSteer stores a message the user sent the subagent.
func (s *Store) AddSubagentSteer(ctx context.Context, k hub.SubagentKey, text string, at time.Time) (hub.SubagentEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return hub.SubagentEvent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, _, next, err := s.ensureLog(ctx, tx, k)
	if err != nil {
		return hub.SubagentEvent{}, err
	}
	evs := []hub.SubagentEvent{{TS: at.UTC().Truncate(time.Millisecond), Type: hub.StepSteer, Text: text}}
	if next, err = s.insertEvents(ctx, tx, k, next, evs); err != nil {
		return hub.SubagentEvent{}, err
	}
	if _, err := tx.ExecContext(ctx, s.q(`UPDATE subagent_logs SET next_seq = ? WHERE `+subagentKeyWhere), append([]any{next}, keyArgs(k)...)...); err != nil {
		return hub.SubagentEvent{}, err
	}
	return evs[0], tx.Commit()
}

// PruneSubagentLogs drops the logs not written since t.
func (s *Store) PruneSubagentLogs(ctx context.Context, t time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM subagent_events WHERE EXISTS (SELECT 1 FROM subagent_logs l
		WHERE l."user" = subagent_events."user" AND l.backend_id = subagent_events.backend_id AND l.session_id = subagent_events.session_id
		AND l.subagent_id = subagent_events.subagent_id AND l.updated_at < ?)`), t.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM subagent_logs WHERE updated_at < ?`), t.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
