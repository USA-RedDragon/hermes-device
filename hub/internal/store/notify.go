package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.NotifyStore = (*Store)(nil)

// NotifySettings returns a person's settings, or the defaults.
func (s *Store) NotifySettings(ctx context.Context, user string) (hub.NotifySettings, error) {
	var raw string
	err := s.row(ctx, `SELECT settings FROM notify_settings WHERE "user" = ?`, user).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.DefaultNotifySettings(), nil
	}
	if err != nil {
		return hub.NotifySettings{}, err
	}
	out := hub.DefaultNotifySettings()
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return hub.NotifySettings{}, err
	}
	return out, nil
}

// SetNotifySettings stores a person's settings.
func (s *Store) SetNotifySettings(ctx context.Context, user string, ns hub.NotifySettings) error {
	b, err := json.Marshal(ns)
	if err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO notify_settings ("user", settings, updated_at) VALUES (?, ?, ?)
		ON CONFLICT("user") DO UPDATE SET settings = excluded.settings, updated_at = excluded.updated_at`,
		user, string(b), s.now().Unix())
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const noticeColumns = `id, "user", dedupe_key, kind, backend_id, session_id, prompt_id, prompt, created_at, settled_at, account_id`

func scanNotice(sc scanner) (hub.NoticeRecord, error) {
	var (
		r                hub.NoticeRecord
		prompt           string
		created, settled int64
	)
	if err := sc.Scan(&r.ID, &r.User, &r.Key, &r.Kind, &r.BackendID, &r.SessionID, &r.PromptID, &prompt, &created, &settled, &r.Account); err != nil {
		return r, err
	}
	if prompt != "" {
		var p hub.PromptInfo
		if json.Unmarshal([]byte(prompt), &p) == nil {
			r.Prompt = &p
		}
	}
	r.CreatedAt = time.Unix(created, 0)
	if settled > 0 {
		r.SettledAt = time.Unix(settled, 0)
	}
	return r, nil
}

// ClaimNotice stores r unless the person already has a notice with r.Key.
func (s *Store) ClaimNotice(ctx context.Context, r hub.NoticeRecord) (string, bool, error) {
	id, err := newID()
	if err != nil {
		return "", false, err
	}
	prompt := ""
	if r.Prompt != nil {
		b, err := json.Marshal(r.Prompt)
		if err != nil {
			return "", false, err
		}
		prompt = string(b)
	}
	res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO notices (id, "user", dedupe_key, kind, backend_id, session_id, prompt_id, prompt,
		created_at, account_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT("user", dedupe_key) DO NOTHING`),
		id, r.User, r.Key, r.Kind, r.BackendID, r.SessionID, r.PromptID, prompt, s.now().Unix(), r.Account)
	if err != nil {
		return "", false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return id, err == nil, err
	}
	err = s.row(ctx, `SELECT id FROM notices WHERE "user" = ? AND dedupe_key = ?`, r.User, r.Key).Scan(&id)
	return id, false, err
}

// Notice returns a stored notice.
func (s *Store) Notice(ctx context.Context, id string) (hub.NoticeRecord, bool, error) {
	r, err := scanNotice(s.row(ctx, `SELECT `+noticeColumns+` FROM notices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return hub.NoticeRecord{}, false, nil
	}
	return r, err == nil, err
}

// SentSince counts a person's notices since t.
func (s *Store) SentSince(ctx context.Context, user string, t time.Time) (int, error) {
	var n int
	err := s.row(ctx, `SELECT COUNT(*) FROM notices WHERE "user" = ? AND created_at >= ?`, user, t.Unix()).Scan(&n)
	return n, err
}

func (s *Store) notices(ctx context.Context, query string, args ...any) ([]hub.NoticeRecord, error) {
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hub.NoticeRecord
	for rows.Next() {
		r, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SettleNotices marks the open notices of the prompts settled and returns
// them.
func (s *Store) SettleNotices(ctx context.Context, backendID, sessionID string, promptIDs []string) ([]hub.NoticeRecord, error) {
	if len(promptIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(promptIDs)), ", ")
	args := []any{s.now().Unix(), backendID, sessionID}
	for _, id := range promptIDs {
		args = append(args, id)
	}
	return s.notices(ctx, `UPDATE notices SET settled_at = ? WHERE settled_at = 0 AND prompt_id <> ''
		AND backend_id = ? AND session_id = ? AND prompt_id IN (`+marks+`) RETURNING `+noticeColumns, args...)
}

// DueReminders lists open approval and question notices created before
// t that had no reminder yet and are not put off past now.
func (s *Store) DueReminders(ctx context.Context, t, now time.Time) ([]hub.NoticeRecord, error) {
	return s.notices(ctx, `SELECT `+noticeColumns+` FROM notices WHERE settled_at = 0 AND reminded_at = 0
		AND kind IN (?, ?) AND created_at <= ? AND next_at <= ? ORDER BY created_at LIMIT 100`,
		hub.NoticeApproval, hub.NoticeQuestion, t.Unix(), now.Unix())
}

// DeferReminder puts a notice's reminder off until t.
func (s *Store) DeferReminder(ctx context.Context, id string, t time.Time) error {
	return s.exec(ctx, `UPDATE notices SET next_at = ? WHERE id = ?`, t.Unix(), id)
}

// ClaimReminder marks a notice reminded; false when it already was.
func (s *Store) ClaimReminder(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE notices SET reminded_at = ? WHERE id = ? AND reminded_at = 0`), s.now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AddNoticeMessage records a Telegram message that shows a notice.
func (s *Store) AddNoticeMessage(ctx context.Context, id string, m hub.NoticeMessage) error {
	return s.exec(ctx, `INSERT INTO notice_messages (notice_id, chat_id, message_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		id, m.ChatID, m.MessageID)
}

// NoticeForMessage is the notice that a Telegram message shows.
func (s *Store) NoticeForMessage(ctx context.Context, chatID, messageID int64) (string, bool, error) {
	var id string
	err := s.row(ctx, `SELECT notice_id FROM notice_messages WHERE chat_id = ? AND message_id = ?`, chatID, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// NoticeMessages lists the Telegram messages that show a notice.
func (s *Store) NoticeMessages(ctx context.Context, id string) ([]hub.NoticeMessage, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT chat_id, message_id FROM notice_messages WHERE notice_id = ? ORDER BY message_id`), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hub.NoticeMessage
	for rows.Next() {
		var m hub.NoticeMessage
		if err := rows.Scan(&m.ChatID, &m.MessageID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneNotices drops notices, their messages and presence older than t.
func (s *Store) PruneNotices(ctx context.Context, t time.Time) error {
	if err := s.exec(ctx, `DELETE FROM notice_messages WHERE notice_id IN (SELECT id FROM notices WHERE created_at < ?)`, t.Unix()); err != nil {
		return err
	}
	if err := s.exec(ctx, `DELETE FROM notices WHERE created_at < ?`, t.Unix()); err != nil {
		return err
	}
	return s.exec(ctx, `DELETE FROM presence WHERE seen_at < ?`, t.Unix())
}

// ErrPushInUse is a subscription of another person that names other keys.
var ErrPushInUse = errors.New("push subscription belongs to someone else")

// SavePushSubscription stores a browser's subscription. A browser that
// subscribes again keeps one row; it moves to the person who subscribed
// last, since only one person can be signed in to it, but only when it
// names the same keys, which only that browser has.
func (s *Store) SavePushSubscription(ctx context.Context, ps hub.PushSubscription) (hub.PushSubscription, error) {
	id, err := newID()
	if err != nil {
		return ps, err
	}
	now := s.now()
	err = s.row(ctx, `INSERT INTO push_subscriptions (id, "user", endpoint, p256dh, auth, label, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET "user" = excluded."user", p256dh = excluded.p256dh, auth = excluded.auth,
		label = excluded.label, created_at = excluded.created_at
		WHERE push_subscriptions."user" = excluded."user" OR (push_subscriptions.p256dh = excluded.p256dh AND push_subscriptions.auth = excluded.auth)
		RETURNING id`,
		id, ps.User, ps.Endpoint, ps.P256dh, ps.Auth, ps.Label, now.Unix()).Scan(&ps.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return ps, ErrPushInUse
	}
	ps.Created = time.Unix(now.Unix(), 0)
	return ps, err
}

// PushSubscriptions lists a person's subscriptions, newest first.
func (s *Store) PushSubscriptions(ctx context.Context, user string) ([]hub.PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, "user", endpoint, p256dh, auth, label, created_at FROM push_subscriptions
		WHERE "user" = ? ORDER BY created_at DESC, id`), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hub.PushSubscription
	for rows.Next() {
		var (
			ps      hub.PushSubscription
			created int64
		)
		if err := rows.Scan(&ps.ID, &ps.User, &ps.Endpoint, &ps.P256dh, &ps.Auth, &ps.Label, &created); err != nil {
			return nil, err
		}
		ps.Created = time.Unix(created, 0)
		out = append(out, ps)
	}
	return out, rows.Err()
}

// DeletePushSubscription deletes a person's subscription by ID or endpoint.
func (s *Store) DeletePushSubscription(ctx context.Context, user, idOrEndpoint string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM push_subscriptions WHERE "user" = ? AND (id = ? OR endpoint = ?)`),
		user, idOrEndpoint, idOrEndpoint)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetPresence records whether user has the chat open in a visible page.
func (s *Store) SetPresence(ctx context.Context, user, backendID, sessionID string, visible bool) error {
	if !visible {
		return s.exec(ctx, `DELETE FROM presence WHERE "user" = ? AND backend_id = ? AND session_id = ?`, user, backendID, sessionID)
	}
	if err := s.exec(ctx, `DELETE FROM presence WHERE "user" = ? AND seen_at < ?`, user, s.now().Add(-presenceTTL).Unix()); err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO presence ("user", backend_id, session_id, seen_at) VALUES (?, ?, ?, ?)
		ON CONFLICT("user", backend_id, session_id) DO UPDATE SET seen_at = excluded.seen_at`, user, backendID, sessionID, s.now().Unix())
}

// presenceTTL is how long the store keeps a page's visit. Pages report
// every 45 seconds; older visits no longer count.
const presenceTTL = 5 * time.Minute

// Present reports whether user had the chat open in a visible page since t.
func (s *Store) Present(ctx context.Context, user, backendID, sessionID string, t time.Time) (bool, error) {
	var n int
	err := s.row(ctx, `SELECT COUNT(*) FROM presence WHERE "user" = ? AND backend_id = ? AND session_id = ? AND seen_at >= ?`,
		user, backendID, sessionID, t.Unix()).Scan(&n)
	return n > 0, err
}

// TelegramIDs lists the Telegram accounts linked to user.
func (s *Store) TelegramIDs(ctx context.Context, user string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT telegram_id FROM telegram_links WHERE "user" = ? ORDER BY telegram_id`), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TelegramAttached reports whether one of user's Telegram chats is
// attached to the session.
func (s *Store) TelegramAttached(ctx context.Context, user, backendID, sessionID string) (bool, error) {
	var n int
	err := s.row(ctx, `SELECT (SELECT COUNT(*) FROM telegram_chats WHERE "user" = ? AND backend_id = ? AND session_id = ?)
		+ (SELECT COUNT(*) FROM telegram_topics t JOIN telegram_groups g ON g.chat_id = t.chat_id AND g."user" = t."user"
		WHERE t."user" = ? AND t.backend_id = ? AND t.session_id = ? AND t.thread_id <> 0)`,
		user, backendID, sessionID, user, backendID, sessionID).Scan(&n)
	return n > 0, err
}

// TelegramGroup is the forum group user linked.
func (s *Store) TelegramGroup(ctx context.Context, user string) (int64, bool, error) {
	var id int64
	err := s.row(ctx, `SELECT chat_id FROM telegram_groups WHERE "user" = ?`, user).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// CronMark is the last run the hub saw of a scheduled job.
func (s *Store) CronMark(ctx context.Context, backendID, jobID string) (string, bool, error) {
	var mark string
	err := s.row(ctx, `SELECT mark FROM cron_marks WHERE backend_id = ? AND job_id = ?`, backendID, jobID).Scan(&mark)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return mark, err == nil, err
}

// SetCronMark records the last run the hub saw of a scheduled job.
func (s *Store) SetCronMark(ctx context.Context, backendID, jobID, mark string) error {
	return s.exec(ctx, `INSERT INTO cron_marks (backend_id, job_id, mark) VALUES (?, ?, ?)
		ON CONFLICT(backend_id, job_id) DO UPDATE SET mark = excluded.mark`, backendID, jobID, mark)
}
