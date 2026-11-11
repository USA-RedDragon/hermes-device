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
	{
		version: 3,
		sqlite: []string{
			`CREATE TABLE telegram_chats (chat_id INTEGER PRIMARY KEY, "user" TEXT NOT NULL, backend_id TEXT NOT NULL, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
			`CREATE TABLE telegram_offsets (bot_id INTEGER PRIMARY KEY, next_offset INTEGER NOT NULL)`,
		},
		postgres: []string{
			`CREATE TABLE telegram_chats (chat_id BIGINT PRIMARY KEY, "user" TEXT NOT NULL, backend_id TEXT NOT NULL, session_id TEXT NOT NULL, updated_at BIGINT NOT NULL)`,
			`CREATE TABLE telegram_offsets (bot_id BIGINT PRIMARY KEY, next_offset BIGINT NOT NULL)`,
		},
	},
	{
		version: 4,
		sqlite: []string{
			`CREATE TABLE devices (id TEXT PRIMARY KEY, "user" TEXT NOT NULL, backend_id TEXT NOT NULL, name TEXT NOT NULL,
				hostname TEXT NOT NULL, os TEXT NOT NULL, arch TEXT NOT NULL, client_version TEXT NOT NULL, token_hash TEXT UNIQUE,
				created_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL DEFAULT 0, last_addr TEXT NOT NULL DEFAULT '',
				prev_addr TEXT NOT NULL DEFAULT '', addr_changed_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0)`,
			`CREATE UNIQUE INDEX devices_user_name ON devices ("user", name) WHERE revoked_at = 0`,
			`CREATE TABLE device_pairings (device_code_hash TEXT PRIMARY KEY, user_code TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
				hostname TEXT NOT NULL, os TEXT NOT NULL, arch TEXT NOT NULL, client_version TEXT NOT NULL, addr TEXT NOT NULL,
				created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, last_poll_ms INTEGER NOT NULL DEFAULT 0,
				status TEXT NOT NULL, device_id TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE device_code_failures ("user" TEXT NOT NULL, at INTEGER NOT NULL)`,
			`CREATE INDEX device_code_failures_user ON device_code_failures ("user", at)`,
		},
		postgres: []string{
			`CREATE TABLE devices (id TEXT PRIMARY KEY, "user" TEXT NOT NULL, backend_id TEXT NOT NULL, name TEXT NOT NULL,
				hostname TEXT NOT NULL, os TEXT NOT NULL, arch TEXT NOT NULL, client_version TEXT NOT NULL, token_hash TEXT UNIQUE,
				created_at BIGINT NOT NULL, last_seen_at BIGINT NOT NULL DEFAULT 0, last_addr TEXT NOT NULL DEFAULT '',
				prev_addr TEXT NOT NULL DEFAULT '', addr_changed_at BIGINT NOT NULL DEFAULT 0, revoked_at BIGINT NOT NULL DEFAULT 0)`,
			`CREATE UNIQUE INDEX devices_user_name ON devices ("user", name) WHERE revoked_at = 0`,
			`CREATE TABLE device_pairings (device_code_hash TEXT PRIMARY KEY, user_code TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
				hostname TEXT NOT NULL, os TEXT NOT NULL, arch TEXT NOT NULL, client_version TEXT NOT NULL, addr TEXT NOT NULL,
				created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, last_poll_ms BIGINT NOT NULL DEFAULT 0,
				status TEXT NOT NULL, device_id TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE device_code_failures ("user" TEXT NOT NULL, at BIGINT NOT NULL)`,
			`CREATE INDEX device_code_failures_user ON device_code_failures ("user", at)`,
		},
	},
	{
		version:  5,
		sqlite:   []string{`CREATE TABLE device_dirs (device_id TEXT NOT NULL, dir TEXT NOT NULL, used_at INTEGER NOT NULL, PRIMARY KEY (device_id, dir))`},
		postgres: []string{`CREATE TABLE device_dirs (device_id TEXT NOT NULL, dir TEXT NOT NULL, used_at BIGINT NOT NULL, PRIMARY KEY (device_id, dir))`},
	},
	{
		version: 6,
		sqlite: []string{
			`ALTER TABLE devices ADD COLUMN exec_leg TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE devices ADD COLUMN close_code INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE devices ADD COLUMN close_reason TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE devices ADD COLUMN close_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE devices ADD COLUMN closed_at INTEGER NOT NULL DEFAULT 0`,
		},
		postgres: []string{
			`ALTER TABLE devices ADD COLUMN exec_leg TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE devices ADD COLUMN close_code INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE devices ADD COLUMN close_reason TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE devices ADD COLUMN close_by TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE devices ADD COLUMN closed_at BIGINT NOT NULL DEFAULT 0`,
		},
	},
	{
		version: 7,
		sqlite: []string{
			`CREATE TABLE browser_sessions (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, "user" TEXT NOT NULL,
				device_id TEXT NOT NULL, instance_id TEXT NOT NULL, browser TEXT NOT NULL, created_at INTEGER NOT NULL,
				last_used_at INTEGER NOT NULL, revoked_at INTEGER NOT NULL DEFAULT 0)`,
			`CREATE UNIQUE INDEX browser_sessions_instance ON browser_sessions (device_id, instance_id) WHERE revoked_at = 0`,
			`CREATE TABLE browser_codes (code_hash TEXT PRIMARY KEY, "user" TEXT NOT NULL, device_id TEXT NOT NULL,
				instance_id TEXT NOT NULL, browser TEXT NOT NULL, challenge TEXT NOT NULL, redirect_uri TEXT NOT NULL,
				expires_at INTEGER NOT NULL)`,
		},
		postgres: []string{
			`CREATE TABLE browser_sessions (id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, "user" TEXT NOT NULL,
				device_id TEXT NOT NULL, instance_id TEXT NOT NULL, browser TEXT NOT NULL, created_at BIGINT NOT NULL,
				last_used_at BIGINT NOT NULL, revoked_at BIGINT NOT NULL DEFAULT 0)`,
			`CREATE UNIQUE INDEX browser_sessions_instance ON browser_sessions (device_id, instance_id) WHERE revoked_at = 0`,
			`CREATE TABLE browser_codes (code_hash TEXT PRIMARY KEY, "user" TEXT NOT NULL, device_id TEXT NOT NULL,
				instance_id TEXT NOT NULL, browser TEXT NOT NULL, challenge TEXT NOT NULL, redirect_uri TEXT NOT NULL,
				expires_at BIGINT NOT NULL)`,
		},
	},
	{
		version: 8,
		sqlite: []string{
			`CREATE TABLE agent_messages (id TEXT PRIMARY KEY, backend_id TEXT NOT NULL, from_session TEXT NOT NULL,
				from_root TEXT NOT NULL, from_subagent TEXT NOT NULL, from_name TEXT NOT NULL, to_session TEXT NOT NULL,
				to_subagent TEXT NOT NULL, to_name TEXT NOT NULL, reply_to TEXT NOT NULL, hop INTEGER NOT NULL,
				text TEXT NOT NULL, mode TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL)`,
			`CREATE INDEX agent_messages_from ON agent_messages (backend_id, from_root, created_at)`,
			`CREATE INDEX agent_messages_to ON agent_messages (backend_id, to_session, created_at)`,
			`CREATE TABLE agent_names (backend_id TEXT NOT NULL, session_id TEXT NOT NULL, name TEXT NOT NULL,
				name_key TEXT NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY (backend_id, session_id))`,
			`CREATE UNIQUE INDEX agent_names_key ON agent_names (backend_id, name_key)`,
		},
		postgres: []string{
			`CREATE TABLE agent_messages (id TEXT PRIMARY KEY, backend_id TEXT NOT NULL, from_session TEXT NOT NULL,
				from_root TEXT NOT NULL, from_subagent TEXT NOT NULL, from_name TEXT NOT NULL, to_session TEXT NOT NULL,
				to_subagent TEXT NOT NULL, to_name TEXT NOT NULL, reply_to TEXT NOT NULL, hop BIGINT NOT NULL,
				text TEXT NOT NULL, mode TEXT NOT NULL, status TEXT NOT NULL, created_at BIGINT NOT NULL)`,
			`CREATE INDEX agent_messages_from ON agent_messages (backend_id, from_root, created_at)`,
			`CREATE INDEX agent_messages_to ON agent_messages (backend_id, to_session, created_at)`,
			`CREATE TABLE agent_names (backend_id TEXT NOT NULL, session_id TEXT NOT NULL, name TEXT NOT NULL,
				name_key TEXT NOT NULL, updated_at BIGINT NOT NULL, PRIMARY KEY (backend_id, session_id))`,
			`CREATE UNIQUE INDEX agent_names_key ON agent_names (backend_id, name_key)`,
		},
	},
	{
		version:  9,
		sqlite:   []string{`DROP TABLE browser_codes`, `DROP TABLE browser_sessions`},
		postgres: []string{`DROP TABLE browser_codes`, `DROP TABLE browser_sessions`},
	},
	{
		version: 10,
		sqlite: []string{
			`CREATE TABLE notify_settings ("user" TEXT PRIMARY KEY, settings TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
			`CREATE TABLE notices (id TEXT PRIMARY KEY, "user" TEXT NOT NULL, dedupe_key TEXT NOT NULL, kind TEXT NOT NULL,
				backend_id TEXT NOT NULL, session_id TEXT NOT NULL, prompt_id TEXT NOT NULL, prompt TEXT NOT NULL,
				created_at INTEGER NOT NULL, settled_at INTEGER NOT NULL DEFAULT 0, reminded_at INTEGER NOT NULL DEFAULT 0,
				next_at INTEGER NOT NULL DEFAULT 0, UNIQUE ("user", dedupe_key))`,
			`CREATE INDEX notices_prompt ON notices (backend_id, session_id, prompt_id)`,
			`CREATE INDEX notices_created ON notices (created_at)`,
			`CREATE TABLE notice_messages (notice_id TEXT NOT NULL, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
				PRIMARY KEY (notice_id, chat_id, message_id))`,
			`CREATE TABLE push_subscriptions (id TEXT PRIMARY KEY, "user" TEXT NOT NULL, endpoint TEXT NOT NULL UNIQUE,
				p256dh TEXT NOT NULL, auth TEXT NOT NULL, label TEXT NOT NULL, created_at INTEGER NOT NULL)`,
			`CREATE INDEX push_subscriptions_user ON push_subscriptions ("user")`,
			`CREATE TABLE presence ("user" TEXT NOT NULL, backend_id TEXT NOT NULL, session_id TEXT NOT NULL, seen_at INTEGER NOT NULL,
				PRIMARY KEY ("user", backend_id, session_id))`,
			`CREATE TABLE cron_marks (backend_id TEXT NOT NULL, job_id TEXT NOT NULL, mark TEXT NOT NULL, PRIMARY KEY (backend_id, job_id))`,
		},
		postgres: []string{
			`CREATE TABLE notify_settings ("user" TEXT PRIMARY KEY, settings TEXT NOT NULL, updated_at BIGINT NOT NULL)`,
			`CREATE TABLE notices (id TEXT PRIMARY KEY, "user" TEXT NOT NULL, dedupe_key TEXT NOT NULL, kind TEXT NOT NULL,
				backend_id TEXT NOT NULL, session_id TEXT NOT NULL, prompt_id TEXT NOT NULL, prompt TEXT NOT NULL,
				created_at BIGINT NOT NULL, settled_at BIGINT NOT NULL DEFAULT 0, reminded_at BIGINT NOT NULL DEFAULT 0,
				next_at BIGINT NOT NULL DEFAULT 0, UNIQUE ("user", dedupe_key))`,
			`CREATE INDEX notices_prompt ON notices (backend_id, session_id, prompt_id)`,
			`CREATE INDEX notices_created ON notices (created_at)`,
			`CREATE TABLE notice_messages (notice_id TEXT NOT NULL, chat_id BIGINT NOT NULL, message_id BIGINT NOT NULL,
				PRIMARY KEY (notice_id, chat_id, message_id))`,
			`CREATE TABLE push_subscriptions (id TEXT PRIMARY KEY, "user" TEXT NOT NULL, endpoint TEXT NOT NULL UNIQUE,
				p256dh TEXT NOT NULL, auth TEXT NOT NULL, label TEXT NOT NULL, created_at BIGINT NOT NULL)`,
			`CREATE INDEX push_subscriptions_user ON push_subscriptions ("user")`,
			`CREATE TABLE presence ("user" TEXT NOT NULL, backend_id TEXT NOT NULL, session_id TEXT NOT NULL, seen_at BIGINT NOT NULL,
				PRIMARY KEY ("user", backend_id, session_id))`,
			`CREATE TABLE cron_marks (backend_id TEXT NOT NULL, job_id TEXT NOT NULL, mark TEXT NOT NULL, PRIMARY KEY (backend_id, job_id))`,
		},
	},
	{
		version: 11,
		sqlite: []string{
			`CREATE TABLE telegram_groups ("user" TEXT PRIMARY KEY, chat_id INTEGER NOT NULL UNIQUE, linked_at INTEGER NOT NULL)`,
			`CREATE TABLE telegram_topics (chat_id INTEGER NOT NULL, backend_id TEXT NOT NULL, session_id TEXT NOT NULL,
				thread_id INTEGER NOT NULL, "user" TEXT NOT NULL, name TEXT NOT NULL, seen_at INTEGER NOT NULL,
				PRIMARY KEY (chat_id, backend_id, session_id))`,
		},
		postgres: []string{
			`CREATE TABLE telegram_groups ("user" TEXT PRIMARY KEY, chat_id BIGINT NOT NULL UNIQUE, linked_at BIGINT NOT NULL)`,
			`CREATE TABLE telegram_topics (chat_id BIGINT NOT NULL, backend_id TEXT NOT NULL, session_id TEXT NOT NULL,
				thread_id BIGINT NOT NULL, "user" TEXT NOT NULL, name TEXT NOT NULL, seen_at BIGINT NOT NULL,
				PRIMARY KEY (chat_id, backend_id, session_id))`,
		},
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
