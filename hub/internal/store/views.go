package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.ViewCache = (*Store)(nil)

// viewKeep is how long the hub keeps a copy no one saved again.
const viewKeep = 30 * 24 * time.Hour

// SaveView keeps a backend's chat list or a chat's newest rows. A copy
// older than the one kept is dropped, so two replicas that save at once
// keep the newer one. Saving a list also drops the copies of chats no
// one opened for a long time.
func (s *Store) SaveView(ctx context.Context, backendID, key string, data []byte, at time.Time) error {
	err := s.exec(ctx, `INSERT INTO view_cache (backend_id, key, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (backend_id, key) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at
		WHERE view_cache.updated_at <= excluded.updated_at`, backendID, key, string(data), at.UnixMilli())
	if err != nil || key != hub.ViewListKey {
		return err
	}
	return s.exec(ctx, `DELETE FROM view_cache WHERE updated_at < ?`, s.now().Add(-viewKeep).UnixMilli())
}

// View returns what SaveView kept.
func (s *Store) View(ctx context.Context, backendID, key string) ([]byte, time.Time, bool, error) {
	var (
		data string
		at   int64
	)
	err := s.row(ctx, `SELECT data, updated_at FROM view_cache WHERE backend_id = ? AND key = ?`, backendID, key).Scan(&data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return []byte(data), time.UnixMilli(at), true, nil
}
