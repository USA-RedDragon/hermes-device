package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.BrowserStore = (*Store)(nil)

const browserColumns = `b.id, b."user", b.device_id, b.instance_id, b.browser, b.created_at, b.last_used_at`

// activeBrowser limits a query on browser_sessions b to active sign-ins.
const activeBrowser = `b.revoked_at = 0 AND b.last_used_at > ?
	AND EXISTS (SELECT 1 FROM devices d WHERE d.id = b.device_id AND d.revoked_at = 0)`

func scanBrowser(r scanner) (hub.BrowserSession, error) {
	var b hub.BrowserSession
	var created, used int64
	if err := r.Scan(&b.ID, &b.User, &b.DeviceID, &b.InstanceID, &b.Browser, &created, &used); err != nil {
		return hub.BrowserSession{}, err
	}
	b.CreatedAt, b.LastUsedAt = time.Unix(created, 0), time.Unix(used, 0)
	return b, nil
}

func (s *Store) idleCutoff() int64 { return s.now().Add(-hub.BrowserIdleTTL).Unix() }

// CreateBrowserCode stores a consent grant. See hub.BrowserStore.
func (s *Store) CreateBrowserCode(ctx context.Context, g hub.BrowserGrant) (string, error) {
	now := s.now().Unix()
	if err := s.exec(ctx, `DELETE FROM browser_codes WHERE expires_at <= ?`, now); err != nil {
		return "", err
	}
	code := randomURL(32)
	err := s.exec(ctx, `INSERT INTO browser_codes (code_hash, "user", device_id, instance_id, browser, challenge, redirect_uri, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, tokenHash(code), g.User, g.DeviceID, g.InstanceID, g.Browser, g.Challenge, g.RedirectURI,
		now+int64(hub.BrowserCodeTTL/time.Second))
	if err != nil {
		return "", err
	}
	return code, nil
}

// RedeemBrowserCode uses up a code. See hub.BrowserStore.
func (s *Store) RedeemBrowserCode(ctx context.Context, code string) (hub.BrowserGrant, bool, error) {
	var g hub.BrowserGrant
	var expires int64
	err := s.row(ctx, `DELETE FROM browser_codes WHERE code_hash = ?
		RETURNING "user", device_id, instance_id, browser, challenge, redirect_uri, expires_at`, tokenHash(code)).
		Scan(&g.User, &g.DeviceID, &g.InstanceID, &g.Browser, &g.Challenge, &g.RedirectURI, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.BrowserGrant{}, false, nil
	}
	if err != nil {
		return hub.BrowserGrant{}, false, err
	}
	if expires <= s.now().Unix() {
		return hub.BrowserGrant{}, false, nil
	}
	return g, true, nil
}

// CreateBrowserSession signs a browser in. See hub.BrowserStore.
func (s *Store) CreateBrowserSession(ctx context.Context, user, deviceID, instanceID, browser string) (string, hub.BrowserSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", hub.BrowserSession{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().Unix()
	if _, err := tx.ExecContext(ctx, s.q(`UPDATE browser_sessions SET revoked_at = ?
		WHERE device_id = ? AND instance_id = ? AND revoked_at = 0`), now, deviceID, instanceID); err != nil {
		return "", hub.BrowserSession{}, err
	}
	token := protocol.BrowserTokenPrefix + randomURL(32)
	b := hub.BrowserSession{ID: "b_" + randomURL(12), User: user, DeviceID: deviceID, InstanceID: instanceID, Browser: browser,
		CreatedAt: time.Unix(now, 0), LastUsedAt: time.Unix(now, 0)}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO browser_sessions (id, token_hash, "user", device_id, instance_id, browser,
		created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		b.ID, tokenHash(token), user, deviceID, instanceID, browser, now, now); err != nil {
		return "", hub.BrowserSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", hub.BrowserSession{}, err
	}
	return token, b, nil
}

func (s *Store) oneBrowser(ctx context.Context, where string, arg any) (hub.BrowserSession, bool, error) {
	b, err := scanBrowser(s.row(ctx, `SELECT `+browserColumns+` FROM browser_sessions b WHERE `+where+` AND `+activeBrowser,
		arg, s.idleCutoff()))
	if errors.Is(err, sql.ErrNoRows) {
		return hub.BrowserSession{}, false, nil
	}
	if err != nil {
		return hub.BrowserSession{}, false, err
	}
	return b, true, nil
}

// BrowserSessionByToken finds the sign-in of a token. See hub.BrowserStore.
func (s *Store) BrowserSessionByToken(ctx context.Context, token string) (hub.BrowserSession, bool, error) {
	if !strings.HasPrefix(token, protocol.BrowserTokenPrefix) {
		return hub.BrowserSession{}, false, nil
	}
	return s.oneBrowser(ctx, `b.token_hash = ?`, tokenHash(token))
}

// BrowserSession finds a sign-in by id. See hub.BrowserStore.
func (s *Store) BrowserSession(ctx context.Context, id string) (hub.BrowserSession, bool, error) {
	return s.oneBrowser(ctx, `b.id = ?`, id)
}

// TouchBrowserSession records a use. See hub.BrowserStore.
func (s *Store) TouchBrowserSession(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE browser_sessions SET last_used_at = ? WHERE id = ? AND revoked_at = 0`, s.now().Unix(), id)
}

// BrowserSessions lists a user's sign-ins. See hub.BrowserStore.
func (s *Store) BrowserSessions(ctx context.Context, user string) ([]hub.BrowserSession, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+browserColumns+` FROM browser_sessions b WHERE b."user" = ? AND `+
		activeBrowser+` ORDER BY b.created_at, b.id`), user, s.idleCutoff())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hub.BrowserSession{}
	for rows.Next() {
		b, err := scanBrowser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RevokeBrowserSession revokes one of user's sign-ins. See
// hub.BrowserStore.
func (s *Store) RevokeBrowserSession(ctx context.Context, user, id string) (string, bool, error) {
	var deviceID string
	err := s.row(ctx, `UPDATE browser_sessions SET revoked_at = ? WHERE id = ? AND "user" = ? AND revoked_at = 0
		RETURNING device_id`, s.now().Unix(), id, user).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return deviceID, true, nil
}

// RevokeBrowserToken revokes the sign-in of a token. See hub.BrowserStore.
func (s *Store) RevokeBrowserToken(ctx context.Context, token string) (hub.BrowserSession, bool, error) {
	if !strings.HasPrefix(token, protocol.BrowserTokenPrefix) {
		return hub.BrowserSession{}, false, nil
	}
	b, err := scanBrowser(s.row(ctx, `UPDATE browser_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at = 0
		RETURNING id, "user", device_id, instance_id, browser, created_at, last_used_at`, s.now().Unix(), tokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return hub.BrowserSession{}, false, nil
	}
	if err != nil {
		return hub.BrowserSession{}, false, err
	}
	return b, true, nil
}
