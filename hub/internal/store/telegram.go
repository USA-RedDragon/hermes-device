package store

import (
	"context"
	"database/sql"
	"errors"

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
