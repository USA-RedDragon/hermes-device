package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
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

// SetPinned pins or unpins a chat for user. A chat pinned now goes before
// every other pin; a chat that is pinned already keeps its place; unpinning
// resets the place.
func (s *Store) SetPinned(ctx context.Context, user, backendID, sessionID string, pinned bool) error {
	return s.exec(ctx, `INSERT INTO chat_marks ("user", backend_id, session_id, pinned, pin_pos)
		VALUES (?, ?, ?, ?, (SELECT COALESCE(MIN(pin_pos), 0) - 1 FROM chat_marks WHERE "user" = ? AND pinned = 1))
		ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET pinned = excluded.pinned,
		pin_pos = CASE WHEN excluded.pinned = 0 THEN 0 WHEN chat_marks.pinned = 1 THEN chat_marks.pin_pos ELSE excluded.pin_pos END`,
		user, backendID, sessionID, boolInt(pinned), user)
}

// SetPinOrder puts the pinned chats of user named in order first, in that
// order, as positions 0, 1 and so on. Chats that are not pinned are ignored
// and pinned chats not named follow in their old order. Pins of other
// users are never touched.
func (s *Store) SetPinOrder(ctx context.Context, user string, order []hub.SessionRef) error {
	return s.pinOrder(ctx, user, nil, order)
}

// PinInOrder pins the chat add, if it is not pinned yet, and sets the
// order of the pins as SetPinOrder does, in one transaction: add lands
// where order names it, so a chat dropped into the pinned list never shows
// at the top first.
func (s *Store) PinInOrder(ctx context.Context, user string, add hub.SessionRef, order []hub.SessionRef) error {
	return s.pinOrder(ctx, user, &add, order)
}

func (s *Store) pinOrder(ctx context.Context, user string, add *hub.SessionRef, order []hub.SessionRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if add != nil {
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO chat_marks ("user", backend_id, session_id, pinned, pin_pos) VALUES (?, ?, ?, 1, 0)
			ON CONFLICT ("user", backend_id, session_id) DO UPDATE SET pinned = 1`), user, add.BackendID, add.SessionID); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, s.q(`SELECT backend_id, session_id, pin_pos FROM chat_marks
		WHERE "user" = ? AND pinned = 1 ORDER BY pin_pos, backend_id, session_id`), user)
	if err != nil {
		return err
	}
	var (
		pinned []hub.SessionRef
		pos    = map[hub.SessionRef]int64{}
	)
	for rows.Next() {
		var ref hub.SessionRef
		var p int64
		if err := rows.Scan(&ref.BackendID, &ref.SessionID, &p); err != nil {
			_ = rows.Close()
			return err
		}
		pinned = append(pinned, ref)
		pos[ref] = p
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	final := make([]hub.SessionRef, 0, len(pinned))
	seen := map[hub.SessionRef]bool{}
	for _, ref := range order {
		if _, ok := pos[ref]; ok && !seen[ref] {
			seen[ref] = true
			final = append(final, ref)
		}
	}
	for _, ref := range pinned {
		if !seen[ref] {
			final = append(final, ref)
		}
	}
	for i, ref := range final {
		if pos[ref] == int64(i) {
			continue
		}
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE chat_marks SET pin_pos = ? WHERE "user" = ? AND backend_id = ? AND session_id = ?`),
			i, user, ref.BackendID, ref.SessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT backend_id, session_id, pinned, pin_pos, seen_at, unread, page FROM chat_marks WHERE "user" = ?`), user)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	out := map[hub.SessionRef]hub.ChatMark{}
	for rows.Next() {
		var (
			ref                  hub.SessionRef
			pinned, unread, page int
			seen, pos            int64
		)
		if err := rows.Scan(&ref.BackendID, &ref.SessionID, &pinned, &pos, &seen, &unread, &page); err != nil {
			return nil, time.Time{}, err
		}
		m := hub.ChatMark{Pinned: pinned != 0, PinPos: pos, Unread: unread != 0, Page: page != 0}
		if seen > 0 {
			m.SeenAt = time.UnixMilli(seen)
		}
		out[ref] = m
	}
	return out, time.UnixMilli(base), rows.Err()
}
