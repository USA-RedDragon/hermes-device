package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.TelegramState = (*Store)(nil)

// SaveTelegramChat records the session a Telegram chat is attached to.
func (s *Store) SaveTelegramChat(ctx context.Context, c hub.TelegramChat) error {
	return s.exec(ctx, `INSERT INTO telegram_chats (chat_id, "user", backend_id, session_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET "user" = excluded."user", backend_id = excluded.backend_id,
		session_id = excluded.session_id, updated_at = excluded.updated_at`,
		c.ChatID, c.User, c.BackendID, c.SessionID, s.now().Unix())
}

// TelegramChats returns every recorded chat attachment.
func (s *Store) TelegramChats(ctx context.Context) ([]hub.TelegramChat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id, "user", backend_id, session_id FROM telegram_chats ORDER BY chat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hub.TelegramChat
	for rows.Next() {
		var c hub.TelegramChat
		if err := rows.Scan(&c.ChatID, &c.User, &c.BackendID, &c.SessionID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TelegramOffset returns the next getUpdates offset of a bot, 0 if none.
func (s *Store) TelegramOffset(ctx context.Context, botID int64) (int64, error) {
	var off int64
	err := s.row(ctx, `SELECT next_offset FROM telegram_offsets WHERE bot_id = ?`, botID).Scan(&off)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return off, err
}

// SetTelegramOffset records the next getUpdates offset of a bot.
func (s *Store) SetTelegramOffset(ctx context.Context, botID, offset int64) error {
	return s.exec(ctx, `INSERT INTO telegram_offsets (bot_id, next_offset) VALUES (?, ?)
		ON CONFLICT(bot_id) DO UPDATE SET next_offset = excluded.next_offset`, botID, offset)
}

// LinkTelegramGroup links a forum group to a hub user, in place of the
// user's earlier group, and forgets the topics of both.
func (s *Store) LinkTelegramGroup(ctx context.Context, g hub.TelegramGroup) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM telegram_topics WHERE chat_id IN (SELECT chat_id FROM telegram_groups WHERE "user" = ? OR chat_id = ?)`,
				[]any{g.User, g.ChatID}},
			{`DELETE FROM telegram_groups WHERE "user" = ? OR chat_id = ?`, []any{g.User, g.ChatID}},
			{`DELETE FROM telegram_chats WHERE chat_id = ?`, []any{g.ChatID}},
			{`INSERT INTO telegram_groups ("user", chat_id, linked_at) VALUES (?, ?, ?)`, []any{g.User, g.ChatID, g.LinkedAt.Unix()}},
		} {
			if _, err := tx.ExecContext(ctx, s.q(q.query), q.args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnlinkTelegramGroup forgets a group and its topics.
func (s *Store) UnlinkTelegramGroup(ctx context.Context, chatID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM telegram_topics WHERE chat_id = ?`), chatID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`DELETE FROM telegram_groups WHERE chat_id = ?`), chatID)
		return err
	})
}

// TelegramGroups lists the linked groups.
func (s *Store) TelegramGroups(ctx context.Context) ([]hub.TelegramGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT "user", chat_id, linked_at FROM telegram_groups ORDER BY "user"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hub.TelegramGroup
	for rows.Next() {
		var (
			g  hub.TelegramGroup
			at int64
		)
		if err := rows.Scan(&g.User, &g.ChatID, &at); err != nil {
			return nil, err
		}
		g.LinkedAt = time.Unix(at, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

// SaveTelegramTopic records the topic of a chat in a linked group.
func (s *Store) SaveTelegramTopic(ctx context.Context, t hub.TelegramTopic) error {
	closed := 0
	if t.Closed {
		closed = 1
	}
	return s.exec(ctx, `INSERT INTO telegram_topics (chat_id, backend_id, session_id, thread_id, "user", name, seen_at, closed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, backend_id, session_id) DO UPDATE SET thread_id = excluded.thread_id, "user" = excluded."user",
		name = excluded.name, seen_at = excluded.seen_at, closed = excluded.closed`,
		t.ChatID, t.BackendID, t.SessionID, t.ThreadID, t.User, t.Name, t.Seen.Unix(), closed)
}

// DeleteTelegramTopic forgets the topic of a chat in a linked group.
func (s *Store) DeleteTelegramTopic(ctx context.Context, chatID int64, backendID, sessionID string) error {
	return s.exec(ctx, `DELETE FROM telegram_topics WHERE chat_id = ? AND backend_id = ? AND session_id = ?`, chatID, backendID, sessionID)
}

// TelegramTopics lists the topics of a group.
func (s *Store) TelegramTopics(ctx context.Context, chatID int64) ([]hub.TelegramTopic, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT chat_id, thread_id, "user", backend_id, session_id, name, seen_at, closed
		FROM telegram_topics WHERE chat_id = ? ORDER BY backend_id, session_id`), chatID)
	if err != nil {
		return nil, fmt.Errorf("failed to list topics: %w", err)
	}
	defer rows.Close()
	var out []hub.TelegramTopic
	for rows.Next() {
		var (
			t      hub.TelegramTopic
			seen   int64
			closed int
		)
		if err := rows.Scan(&t.ChatID, &t.ThreadID, &t.User, &t.BackendID, &t.SessionID, &t.Name, &seen, &closed); err != nil {
			return nil, err
		}
		t.Seen, t.Closed = time.Unix(seen, 0), closed != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// tx runs f in a transaction and commits it when f succeeds.
func (s *Store) tx(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
