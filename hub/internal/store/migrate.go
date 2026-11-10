package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is one schema version: statements per dialect, run in order in
// one transaction.
type migration struct {
	version  int
	sqlite   []string
	postgres []string
}

// migrations are the schema versions in order. Version 1 is the schema the
// hub had before it tracked versions, so an older SQLite file adopts it as
// it is. Never edit a released migration: add a new one.
//
//nolint:gochecknoglobals // a fixed table
var migrations = []migration{
	{
		version: 1,
		sqlite: []string{
			`CREATE TABLE IF NOT EXISTS telegram_links (telegram_id INTEGER PRIMARY KEY, "user" TEXT NOT NULL, linked_at INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS link_tokens (token_hash TEXT PRIMARY KEY, telegram_id INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS users (name TEXT PRIMARY KEY, "groups" TEXT NOT NULL, seen_at INTEGER NOT NULL)`,
		},
		postgres: []string{
			`CREATE TABLE IF NOT EXISTS telegram_links (telegram_id BIGINT PRIMARY KEY, "user" TEXT NOT NULL, linked_at BIGINT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS link_tokens (token_hash TEXT PRIMARY KEY, telegram_id BIGINT NOT NULL, expires_at BIGINT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS users (name TEXT PRIMARY KEY, "groups" TEXT NOT NULL, seen_at BIGINT NOT NULL)`,
		},
	},
	{
		version:  2,
		sqlite:   []string{`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`},
		postgres: []string{`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`},
	},
}

// migrateLockKey is the PostgreSQL advisory lock that keeps two replicas
// from migrating at once.
const migrateLockKey = 0x6875622d6d696772 // "hub-migr"

// SchemaVersion is the schema version this build migrates to.
func SchemaVersion() int { return migrations[len(migrations)-1].version }

// migrate brings the schema to SchemaVersion. It refuses a database that a
// newer build already migrated further.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == postgresDialect {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
			return fmt.Errorf("migrate: lock: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if current > SchemaVersion() {
		return fmt.Errorf("migrate: database schema version %d is newer than this build's %d", current, SchemaVersion())
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.apply(ctx, tx, m); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *Store) apply(ctx context.Context, tx *sql.Tx, m migration) error {
	stmts := m.sqlite
	if s.dialect == postgresDialect {
		stmts = m.postgres
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate to version %d: %w", m.version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), m.version, s.now().Unix()); err != nil {
		return fmt.Errorf("migrate to version %d: %w", m.version, err)
	}
	return nil
}
