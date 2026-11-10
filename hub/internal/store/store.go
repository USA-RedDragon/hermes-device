// Package store is the hub's database: an embedded SQLite file for a single
// replica, or PostgreSQL for several. It implements hub.Links,
// hub.TokenStore, hub.TelegramState and hub.DeviceStore and keeps each
// user's last seen groups.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	_ "modernc.org/sqlite"             // database/sql driver "sqlite"
)

// LinkTokenTTL is how long a Telegram link token stays valid.
const LinkTokenTTL = 10 * time.Minute

type dialect int

const (
	sqliteDialect dialect = iota
	postgresDialect
)

// Store is the database.
type Store struct {
	db      *sql.DB
	dialect dialect
	now     func() time.Time
}

var (
	_ hub.Links      = (*Store)(nil)
	_ hub.TokenStore = (*Store)(nil)
)

// Open opens or creates the SQLite database at path. Only one process may
// use it.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	return open(ctx, db, sqliteDialect)
}

// OpenPostgres connects to the PostgreSQL database named by dsn, a URL or
// key=value connection string. Many hub replicas may share it.
func OpenPostgres(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return open(ctx, db, postgresDialect)
}

func open(ctx context.Context, db *sql.DB, d dialect) (*Store, error) {
	s := &Store{db: db, dialect: d, now: time.Now}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Postgres reports whether the store is PostgreSQL, which several replicas
// may share.
func (s *Store) Postgres() bool { return s.dialect == postgresDialect }

// q rewrites the ? placeholders of query for the store's dialect.
func (s *Store) q(query string) string {
	if s.dialect != postgresDialect {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, s.q(query), args...)
	return err
}

func (s *Store) row(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.q(query), args...)
}

// UserForTelegram returns the hub user linked to a Telegram user ID.
func (s *Store) UserForTelegram(ctx context.Context, telegramID int64) (string, bool, error) {
	var user string
	err := s.row(ctx, `SELECT "user" FROM telegram_links WHERE telegram_id = ?`, telegramID).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return user, true, nil
}

// LinkTelegram links a Telegram user ID to a hub user, replacing any
// earlier link of that Telegram ID.
func (s *Store) LinkTelegram(ctx context.Context, telegramID int64, user string) error {
	return s.exec(ctx, `INSERT INTO telegram_links (telegram_id, "user", linked_at) VALUES (?, ?, ?)
		ON CONFLICT(telegram_id) DO UPDATE SET "user" = excluded."user", linked_at = excluded.linked_at`,
		telegramID, user, s.now().Unix())
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateLinkToken returns a new single-use token for telegramID that is
// valid for LinkTokenTTL. Only its hash is stored.
func (s *Store) CreateLinkToken(ctx context.Context, telegramID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := s.now()
	if err := s.exec(ctx, `DELETE FROM link_tokens WHERE expires_at <= ?`, now.Unix()); err != nil {
		return "", err
	}
	err := s.exec(ctx, `INSERT INTO link_tokens (token_hash, telegram_id, expires_at) VALUES (?, ?, ?)`,
		tokenHash(token), telegramID, now.Add(LinkTokenTTL).Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeLinkToken deletes token and returns its Telegram ID. ok is false
// for an unknown, used or expired token.
func (s *Store) ConsumeLinkToken(ctx context.Context, token string) (int64, bool, error) {
	var id, expires int64
	err := s.row(ctx, `DELETE FROM link_tokens WHERE token_hash = ? RETURNING telegram_id, expires_at`,
		tokenHash(token)).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if expires <= s.now().Unix() {
		return 0, false, nil
	}
	return id, true, nil
}

// SetGroups records the groups a user had at their latest login.
func (s *Store) SetGroups(ctx context.Context, user string, groups []string) error {
	if groups == nil {
		groups = []string{}
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO users (name, "groups", seen_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET "groups" = excluded."groups", seen_at = excluded.seen_at`,
		user, string(b), s.now().Unix())
}

// Groups returns the groups a user had at their latest login.
func (s *Store) Groups(ctx context.Context, user string) ([]string, error) {
	var raw string
	err := s.row(ctx, `SELECT "groups" FROM users WHERE name = ?`, user).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var groups []string
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// SetClock replaces the store's clock. For tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }
