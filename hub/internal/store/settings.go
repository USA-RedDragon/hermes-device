package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// settingPrefix keeps hub settings apart from the other meta keys.
const settingPrefix = "setting:"

// Setting returns a hub setting kept in the database; false when unset.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.row(ctx, `SELECT value FROM meta WHERE key = ?`, settingPrefix+key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetSetting keeps a hub setting in the database.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.exec(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingPrefix+key, value)
}

// DeleteSetting removes a hub setting.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	return s.exec(ctx, `DELETE FROM meta WHERE key = ?`, settingPrefix+key)
}

// ErrNoSnapshot is a store that cannot copy itself to a file (PostgreSQL:
// back it up with its own tools).
var ErrNoSnapshot = errors.New("store: only a SQLite store can copy itself to a file")

// Snapshot writes a consistent copy of a SQLite store to path, which must
// not exist yet.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	if s.Postgres() {
		return ErrNoSnapshot
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("store: %s exists", abs)
	}
	_, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, abs)
	return err
}

// ClientVersions lists each paired computer's name and talaria version.
func (s *Store) ClientVersions(ctx context.Context) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT name, client_version FROM devices WHERE revoked_at = 0 AND client_version <> '' ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var d [2]string
		if err := rows.Scan(&d[0], &d[1]); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
