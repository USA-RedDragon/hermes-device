package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.ClaudeCodeStore = (*Store)(nil)

// maxInList is the most values in one IN list.
const maxInList = 500

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// PutClaudeCodeReport records src and replaces its rows of the report's
// hours, and its accounts when the report has any, in one transaction.
func (s *Store) PutClaudeCodeReport(ctx context.Context, src hub.ClaudeCodeSource, r hub.ClaudeCodeReport) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO claude_code_sources (id, kind, "user", backend_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, "user" = excluded."user", backend_id = excluded.backend_id,
		updated_at = excluded.updated_at`), src.ID, src.Kind, src.User, src.BackendID, s.now().Unix()); err != nil {
		return err
	}
	if r.Since > 0 {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM claude_code_usage WHERE source = ? AND hour >= ?`), src.ID, r.Since); err != nil {
			return err
		}
	}
	for i := 0; i < len(r.Hours); i += maxInList {
		hours := r.Hours[i:min(i+maxInList, len(r.Hours))]
		args := []any{src.ID}
		for _, h := range hours {
			args = append(args, h)
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM claude_code_usage WHERE source = ? AND hour IN (`+marks(len(hours))+`)`), //nolint:gosec // only placeholders
			args...); err != nil {
			return err
		}
	}
	ins, err := tx.PrepareContext(ctx, s.q(`INSERT INTO claude_code_usage (source, hour, account_uuid, org_uuid, model, project,
		sidechain, input, output, cache_write, cache_write_1h, cache_read, requests) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, hour, account_uuid, org_uuid, model, project, sidechain) DO UPDATE SET input = excluded.input,
		output = excluded.output, cache_write = excluded.cache_write, cache_write_1h = excluded.cache_write_1h,
		cache_read = excluded.cache_read, requests = excluded.requests`))
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, x := range r.Rows {
		if _, err := ins.ExecContext(ctx, src.ID, x.Hour, x.Account, x.Org, x.Model, x.Project, boolInt(x.Sidechain), x.Input, x.Output,
			x.CacheWrite, x.CacheWrite1h, x.CacheRead, x.Requests); err != nil {
			return err
		}
	}
	if r.Accounts != nil {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM claude_code_accounts WHERE source = ?`), src.ID); err != nil {
			return err
		}
		for _, a := range r.Accounts {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO claude_code_accounts (source, account_uuid, slot, org_uuid, label)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT(source, account_uuid, slot) DO UPDATE SET org_uuid = excluded.org_uuid,
				label = excluded.label`), src.ID, a.UUID, a.Slot, a.Org, a.Label); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ClaudeCodeSources lists every source that sent usage.
func (s *Store) ClaudeCodeSources(ctx context.Context) ([]hub.ClaudeCodeSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, "user", backend_id FROM claude_code_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hub.ClaudeCodeSource
	for rows.Next() {
		var x hub.ClaudeCodeSource
		if err := rows.Scan(&x.ID, &x.Kind, &x.User, &x.BackendID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// inSources runs query, whose last placeholder group is the IN list of
// sources, for each chunk of sources.
func (s *Store) inSources(ctx context.Context, sources []string, query func(n int) string, args []any,
	scan func(*sql.Rows) error,
) error {
	for i := 0; i < len(sources); i += maxInList {
		chunk := sources[i:min(i+maxInList, len(sources))]
		a := append([]any{}, args...)
		for _, x := range chunk {
			a = append(a, x)
		}
		rows, err := s.db.QueryContext(ctx, s.q(query(len(chunk))), a...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := scan(rows); err != nil {
				_ = rows.Close()
				return err
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ClaudeCodeRows returns the rows of sources with from <= hour < to.
func (s *Store) ClaudeCodeRows(ctx context.Context, sources []string, from, to int64) ([]hub.ClaudeCodeRow, error) {
	var out []hub.ClaudeCodeRow
	err := s.inSources(ctx, sources, func(n int) string {
		return `SELECT source, hour, account_uuid, org_uuid, model, project, sidechain, input, output, cache_write, cache_write_1h,
			cache_read, requests FROM claude_code_usage WHERE hour >= ? AND hour < ? AND source IN (` + marks(n) + `)`
	}, []any{from, to}, func(rows *sql.Rows) error {
		var x hub.ClaudeCodeRow
		var side int
		if err := rows.Scan(&x.Source, &x.Hour, &x.Account, &x.Org, &x.Model, &x.Project, &side, &x.Input, &x.Output,
			&x.CacheWrite, &x.CacheWrite1h, &x.CacheRead, &x.Requests); err != nil {
			return err
		}
		x.Sidechain = side != 0
		out = append(out, x)
		return nil
	})
	return out, err
}

// ClaudeCodeAccounts returns the accounts that sources reported.
func (s *Store) ClaudeCodeAccounts(ctx context.Context, sources []string) ([]hub.ClaudeCodeAccount, error) {
	var out []hub.ClaudeCodeAccount
	err := s.inSources(ctx, sources, func(n int) string {
		return `SELECT source, account_uuid, slot, org_uuid, label FROM claude_code_accounts WHERE source IN (` + marks(n) + `)
			ORDER BY source, slot, account_uuid`
	}, nil, func(rows *sql.Rows) error {
		var x hub.ClaudeCodeAccount
		if err := rows.Scan(&x.Source, &x.UUID, &x.Slot, &x.Org, &x.Label); err != nil {
			return err
		}
		out = append(out, x)
		return nil
	})
	return out, err
}

// ClaudeAccountNames maps the account uuids user named to the names.
func (s *Store) ClaudeAccountNames(ctx context.Context, user string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT account_uuid, name FROM claude_account_names WHERE "user" = ?`), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// SetClaudeAccountName names an account for user; "" removes the name.
func (s *Store) SetClaudeAccountName(ctx context.Context, user, accountUUID, name string) error {
	if name == "" {
		return s.exec(ctx, `DELETE FROM claude_account_names WHERE "user" = ? AND account_uuid = ?`, user, accountUUID)
	}
	return s.exec(ctx, `INSERT INTO claude_account_names ("user", account_uuid, name, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT("user", account_uuid) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at`,
		user, accountUUID, name, s.now().Unix())
}
