package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// importMarker is the meta key that records a finished SQLite import.
const importMarker = "sqlite_import"

// importLockKey is the PostgreSQL advisory lock that keeps two replicas
// from importing at once.
const importLockKey = 0x6875622d696d7074 // "hub-impt"

// ErrNotPostgres is returned by ImportSQLite on an SQLite store.
var ErrNotPostgres = errors.New("store: import needs a PostgreSQL store")

// importTables are the tables ImportSQLite copies, with their columns. A
// table the file does not have is skipped.
//
//nolint:gochecknoglobals // a fixed table
var importTables = []struct{ name, columns, key string }{
	{"telegram_links", `telegram_id, "user", linked_at`, "telegram_id"},
	{"link_tokens", "token_hash, telegram_id, expires_at", "token_hash"},
	{"users", `name, "groups", seen_at`, "name"},
	{"telegram_chats", `chat_id, "user", backend_id, session_id, updated_at`, "chat_id"},
	{"telegram_offsets", "bot_id, next_offset", "bot_id"},
	{"devices", `id, "user", backend_id, name, hostname, os, arch, client_version, token_hash, created_at, last_seen_at,
		last_addr, prev_addr, addr_changed_at, revoked_at`, "id"},
	{"device_dirs", "device_id, dir, used_at", "device_id, dir"},
	{"notify_settings", `"user", settings, updated_at`, `"user"`},
	{"push_subscriptions", `id, "user", endpoint, p256dh, auth, label, created_at`, "id"},
}

// ImportSQLite copies every row of the SQLite database at path into the
// PostgreSQL store, once: it does nothing when the file does not exist or
// an import already finished. Rows already in PostgreSQL win. It reports
// how many rows it copied and whether it ran.
func (s *Store) ImportSQLite(ctx context.Context, path string) (int, bool, error) {
	if s.dialect != postgresDialect {
		return 0, false, ErrNotPostgres
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	done, err := s.imported(ctx)
	if err != nil || done {
		return 0, false, err
	}
	src, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, false, fmt.Errorf("import: open %s: %w", path, err)
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(importLockKey)); err != nil {
		return 0, false, fmt.Errorf("import: lock: %w", err)
	}
	var marker string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = $1`, importMarker).Scan(&marker)
	if err == nil {
		return 0, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("import: %w", err)
	}
	n := 0
	for _, t := range importTables {
		copied, err := copyTable(ctx, src, tx, t.name, t.columns, t.key)
		if err != nil {
			return 0, false, fmt.Errorf("import %s: %w", t.name, err)
		}
		n += copied
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ($1, $2)`,
		importMarker, path+" at "+s.now().UTC().Format(time.RFC3339)); err != nil {
		return 0, false, fmt.Errorf("import: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("import: %w", err)
	}
	return n, true, nil
}

func (s *Store) imported(ctx context.Context) (bool, error) {
	var v string
	err := s.row(ctx, `SELECT value FROM meta WHERE key = ?`, importMarker).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func copyTable(ctx context.Context, src *sql.DB, tx *sql.Tx, table, columns, key string) (int, error) {
	var found string
	err := src.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := src.QueryContext(ctx, "SELECT "+columns+" FROM "+table) //nolint:gosec // fixed names from importTables
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	marks := ""
	for i := range cols {
		if i > 0 {
			marks += ", "
		}
		marks += fmt.Sprintf("$%d", i+1)
	}
	insert := "INSERT INTO " + table + " (" + columns + ") VALUES (" + marks + ") ON CONFLICT (" + key + ") DO NOTHING" //nolint:gosec // fixed names
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		res, err := tx.ExecContext(ctx, insert, vals...)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, rows.Err()
}
