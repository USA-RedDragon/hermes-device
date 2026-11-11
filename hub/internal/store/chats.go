package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.ChatStore = (*Store)(nil)

// Draft returns user's draft of a chat: "" when there is none. Times are
// stored in Unix milliseconds.
func (s *Store) Draft(ctx context.Context, user, backendID, sessionID string) (string, time.Time, error) {
	var (
		text string
		at   int64
	)
	err := s.row(ctx, `SELECT text, updated_at FROM chat_drafts WHERE "user" = ? AND backend_id = ? AND session_id = ?`,
		user, backendID, sessionID).Scan(&text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return text, time.UnixMilli(at), nil
}

// SetDraft saves user's draft of a chat; "" deletes it.
func (s *Store) SetDraft(ctx context.Context, user, backendID, sessionID, text string) error {
	if text == "" {
		return s.exec(ctx, `DELETE FROM chat_drafts WHERE "user" = ? AND backend_id = ? AND session_id = ?`, user, backendID, sessionID)
	}
	return s.exec(ctx, `INSERT INTO chat_drafts ("user", backend_id, session_id, text, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET text = excluded.text, updated_at = excluded.updated_at`,
		user, backendID, sessionID, text, s.now().UnixMilli())
}

// SetPinned pins or unpins a chat for user.
func (s *Store) SetPinned(ctx context.Context, user, backendID, sessionID string, pinned bool) error {
	return s.exec(ctx, `INSERT INTO chat_marks ("user", backend_id, session_id, pinned) VALUES (?, ?, ?, ?)
		ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET pinned = excluded.pinned`,
		user, backendID, sessionID, boolInt(pinned))
}

// readSkew is how far past the hub's clock a read time may be, since the
// time the page reports comes from Hermes' clock.
const readSkew = time.Minute

// MarkRead moves user's read time of a chat to at, never back and never
// more than readSkew past now, and clears a hand-set unread mark.
func (s *Store) MarkRead(ctx context.Context, user, backendID, sessionID string, at time.Time) error {
	ms := min(at.UnixMilli(), s.now().Add(readSkew).UnixMilli())
	return s.exec(ctx, `INSERT INTO chat_marks ("user", backend_id, session_id, seen_at) VALUES (?, ?, ?, ?)
		ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET unread = 0,
		seen_at = CASE WHEN chat_marks.seen_at > excluded.seen_at THEN chat_marks.seen_at ELSE excluded.seen_at END`,
		user, backendID, sessionID, max(ms, 0))
}

// MarkUnread marks a chat unread for user by hand.
func (s *Store) MarkUnread(ctx context.Context, user, backendID, sessionID string) error {
	return s.exec(ctx, `INSERT INTO chat_marks ("user", backend_id, session_id, unread) VALUES (?, ?, ?, 1)
		ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET unread = 1`, user, backendID, sessionID)
}

// MarkPage marks a chat of user that the page chat started.
func (s *Store) MarkPage(ctx context.Context, user, backendID, sessionID string) error {
	return s.exec(ctx, `INSERT INTO chat_marks ("user", backend_id, session_id, page) VALUES (?, ?, ?, 1)
		ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET page = 1`, user, backendID, sessionID)
}

// ChatMarks returns user's marks by chat and the user's read baseline,
// which the first call for a user sets to now.
func (s *Store) ChatMarks(ctx context.Context, user string) (map[hub.SessionRef]hub.ChatMark, time.Time, error) {
	if err := s.exec(ctx, `INSERT INTO read_baselines ("user", at) VALUES (?, ?) ON CONFLICT ("user") DO NOTHING`,
		user, s.now().UnixMilli()); err != nil {
		return nil, time.Time{}, err
	}
	var base int64
	if err := s.row(ctx, `SELECT at FROM read_baselines WHERE "user" = ?`, user).Scan(&base); err != nil {
		return nil, time.Time{}, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT backend_id, session_id, pinned, seen_at, unread, page FROM chat_marks WHERE "user" = ?`), user)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	out := map[hub.SessionRef]hub.ChatMark{}
	for rows.Next() {
		var (
			ref                  hub.SessionRef
			pinned, unread, page int
			seen                 int64
		)
		if err := rows.Scan(&ref.BackendID, &ref.SessionID, &pinned, &seen, &unread, &page); err != nil {
			return nil, time.Time{}, err
		}
		m := hub.ChatMark{Pinned: pinned != 0, Unread: unread != 0, Page: page != 0}
		if seen > 0 {
			m.SeenAt = time.UnixMilli(seen)
		}
		out[ref] = m
	}
	return out, time.UnixMilli(base), rows.Err()
}
