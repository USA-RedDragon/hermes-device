package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.DeviceStore = (*Store)(nil)

// TokenPrefix starts every device token (docs/protocol.md 2.1).
const TokenPrefix = "hdt_"

// userCodeAlphabet has no vowels and no letters that look alike, as RFC
// 8628 section 6.1 suggests.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

const (
	pairPending  = "pending"
	pairApproved = "approved"
	pairDenied   = "denied"
)

func randomURL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func newUserCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return string(out)
}

// NormalizeUserCode turns what a user typed into the stored form: the
// alphabet's letters in upper case, a dash after the fourth.
func NormalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(userCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	c := b.String()
	if len(c) != 8 {
		return c
	}
	return c[:4] + "-" + c[4:]
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "SQLSTATE 23505")
}

// cleanup drops expired pairings and the devices of pairings that were
// approved but never fetched their token.
func (s *Store) cleanup(ctx context.Context) error {
	if err := s.exec(ctx, `DELETE FROM device_pairings WHERE expires_at <= ?`, s.now().Unix()); err != nil {
		return err
	}
	if err := s.exec(ctx, `DELETE FROM device_code_failures WHERE at <= ?`, s.now().Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	return s.exec(ctx, `DELETE FROM devices WHERE token_hash IS NULL AND revoked_at = 0
		AND id NOT IN (SELECT device_id FROM device_pairings)`)
}

// StartPairing records a pairing request. See hub.DeviceStore.
func (s *Store) StartPairing(ctx context.Context, req hub.PairingRequest, ttl time.Duration) (string, string, error) {
	if err := s.cleanup(ctx); err != nil {
		return "", "", err
	}
	deviceCode := randomURL(32)
	now := s.now()
	for range 5 {
		userCode := newUserCode()
		err := s.exec(ctx, `INSERT INTO device_pairings (device_code_hash, user_code, name, hostname, os, arch, client_version,
			addr, created_at, expires_at, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tokenHash(deviceCode), userCode, req.Name, req.Hostname, req.OS, req.Arch, req.ClientVersion, req.Addr,
			now.Unix(), now.Add(ttl).Unix(), pairPending)
		if isUnique(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		return deviceCode, userCode, nil
	}
	return "", "", errors.New("start pairing: no free user code")
}

// PendingPairing finds a pending pairing. See hub.DeviceStore.
func (s *Store) PendingPairing(ctx context.Context, userCode string) (hub.Pairing, bool, error) {
	var p hub.Pairing
	var created, expires int64
	err := s.row(ctx, `SELECT user_code, name, hostname, os, arch, client_version, addr, created_at, expires_at
		FROM device_pairings WHERE user_code = ? AND status = ? AND expires_at > ?`,
		NormalizeUserCode(userCode), pairPending, s.now().Unix()).
		Scan(&p.UserCode, &p.Name, &p.Hostname, &p.OS, &p.Arch, &p.ClientVersion, &p.Addr, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.Pairing{}, false, nil
	}
	if err != nil {
		return hub.Pairing{}, false, err
	}
	p.CreatedAt, p.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	return p, true, nil
}

func newDeviceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "d_" + hex.EncodeToString(b)
}

// ApprovePairing creates the device. See hub.DeviceStore.
func (s *Store) ApprovePairing(ctx context.Context, userCode, user, backendID, name string) (hub.Device, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return hub.Device{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now()
	d := hub.Device{ID: newDeviceID(), User: user, BackendID: backendID, Name: name, CreatedAt: time.Unix(now.Unix(), 0)}
	err = tx.QueryRowContext(ctx, s.q(`UPDATE device_pairings SET status = ?, device_id = ?
		WHERE user_code = ? AND status = ? AND expires_at > ? RETURNING hostname, os, arch, client_version`),
		pairApproved, d.ID, NormalizeUserCode(userCode), pairPending, now.Unix()).
		Scan(&d.Hostname, &d.OS, &d.Arch, &d.ClientVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.Device{}, hub.ErrUnknownPairing
	}
	if err != nil {
		return hub.Device{}, err
	}
	_, err = tx.ExecContext(ctx, s.q(`INSERT INTO devices (id, "user", backend_id, name, hostname, os, arch, client_version, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`), d.ID, user, backendID, name, d.Hostname, d.OS, d.Arch, d.ClientVersion, now.Unix())
	if isUnique(err) {
		return hub.Device{}, hub.ErrNameTaken
	}
	if err != nil {
		return hub.Device{}, err
	}
	if err := tx.Commit(); err != nil {
		if isUnique(err) {
			return hub.Device{}, hub.ErrNameTaken
		}
		return hub.Device{}, err
	}
	return d, nil
}

// DenyPairing declines a pending pairing. See hub.DeviceStore.
func (s *Store) DenyPairing(ctx context.Context, userCode string) error {
	return s.exec(ctx, `UPDATE device_pairings SET status = ? WHERE user_code = ? AND status = ?`,
		pairDenied, NormalizeUserCode(userCode), pairPending)
}

// PollPairing answers a device's poll. See hub.DeviceStore.
func (s *Store) PollPairing(ctx context.Context, deviceCode string, interval time.Duration) (hub.PairPoll, error) {
	now := s.now()
	hash := tokenHash(deviceCode)
	var status, deviceID string
	var expires, lastPoll int64
	err := s.row(ctx, `SELECT status, device_id, expires_at, last_poll_ms FROM device_pairings WHERE device_code_hash = ?`, hash).
		Scan(&status, &deviceID, &expires, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.PairPoll{Status: hub.PairExpired}, nil
	}
	if err != nil {
		return hub.PairPoll{}, err
	}
	if expires <= now.Unix() {
		return hub.PairPoll{Status: hub.PairExpired}, s.exec(ctx, `DELETE FROM device_pairings WHERE device_code_hash = ?`, hash)
	}
	slack := min(interval/5, time.Second)
	if lastPoll > 0 && now.UnixMilli()-lastPoll < (interval-slack).Milliseconds() {
		return hub.PairPoll{Status: hub.PairSlowDown},
			s.exec(ctx, `UPDATE device_pairings SET last_poll_ms = ? WHERE device_code_hash = ?`, now.UnixMilli(), hash)
	}
	switch status {
	case pairDenied:
		return hub.PairPoll{Status: hub.PairDenied}, s.exec(ctx, `DELETE FROM device_pairings WHERE device_code_hash = ?`, hash)
	case pairApproved:
		return s.issueToken(ctx, hash)
	}
	return hub.PairPoll{Status: hub.PairPending},
		s.exec(ctx, `UPDATE device_pairings SET last_poll_ms = ? WHERE device_code_hash = ?`, now.UnixMilli(), hash)
}

// issueToken ends an approved pairing and gives its device a token, once.
func (s *Store) issueToken(ctx context.Context, hash string) (hub.PairPoll, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return hub.PairPoll{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var deviceID string
	err = tx.QueryRowContext(ctx, s.q(`DELETE FROM device_pairings WHERE device_code_hash = ? AND status = ? RETURNING device_id`),
		hash, pairApproved).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.PairPoll{Status: hub.PairExpired}, nil
	}
	if err != nil {
		return hub.PairPoll{}, err
	}
	token := TokenPrefix + randomURL(32)
	res, err := tx.ExecContext(ctx, s.q(`UPDATE devices SET token_hash = ? WHERE id = ? AND token_hash IS NULL AND revoked_at = 0`),
		tokenHash(token), deviceID)
	if err != nil {
		return hub.PairPoll{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return hub.PairPoll{Status: hub.PairDenied}, tx.Commit()
	}
	if err := tx.Commit(); err != nil {
		return hub.PairPoll{}, err
	}
	d, ok, err := s.Device(ctx, deviceID)
	if err != nil {
		return hub.PairPoll{}, err
	}
	if !ok {
		return hub.PairPoll{Status: hub.PairDenied}, nil
	}
	return hub.PairPoll{Status: hub.PairApproved, Token: token, Device: d}, nil
}

const deviceColumns = `id, "user", backend_id, name, hostname, os, arch, client_version, token_hash IS NOT NULL,
	created_at, last_seen_at, last_addr, prev_addr, addr_changed_at`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(r scanner) (hub.Device, error) {
	var d hub.Device
	var created, seen, changed int64
	err := r.Scan(&d.ID, &d.User, &d.BackendID, &d.Name, &d.Hostname, &d.OS, &d.Arch, &d.ClientVersion, &d.Paired,
		&created, &seen, &d.LastAddr, &d.PrevAddr, &changed)
	if err != nil {
		return hub.Device{}, err
	}
	d.CreatedAt = time.Unix(created, 0)
	if seen > 0 {
		d.LastSeenAt = time.Unix(seen, 0)
	}
	if changed > 0 {
		d.AddrChangedAt = time.Unix(changed, 0)
	}
	return d, nil
}

func (s *Store) oneDevice(ctx context.Context, where string, args ...any) (hub.Device, bool, error) {
	d, err := scanDevice(s.row(ctx, `SELECT `+deviceColumns+` FROM devices WHERE revoked_at = 0 AND `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return hub.Device{}, false, nil
	}
	if err != nil {
		return hub.Device{}, false, fmt.Errorf("device: %w", err)
	}
	return d, true, nil
}

// DeviceByToken returns the device of a token. See hub.DeviceStore.
func (s *Store) DeviceByToken(ctx context.Context, token string) (hub.Device, bool, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return hub.Device{}, false, nil
	}
	return s.oneDevice(ctx, `token_hash = ?`, tokenHash(token))
}

// Device returns an unrevoked device. See hub.DeviceStore.
func (s *Store) Device(ctx context.Context, id string) (hub.Device, bool, error) {
	return s.oneDevice(ctx, `id = ?`, id)
}

// Devices lists a user's devices. See hub.DeviceStore.
func (s *Store) Devices(ctx context.Context, user string) ([]hub.Device, error) {
	if err := s.cleanup(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+deviceColumns+` FROM devices WHERE revoked_at = 0 AND "user" = ?
		ORDER BY created_at, id`), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hub.Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice revokes a device. See hub.DeviceStore.
func (s *Store) RevokeDevice(ctx context.Context, user, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE devices SET revoked_at = ?, token_hash = NULL
		WHERE id = ? AND "user" = ? AND revoked_at = 0`), s.now().Unix(), id, user)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SeenDevice records a request from a device. See hub.DeviceStore.
func (s *Store) SeenDevice(ctx context.Context, id, addr string) error {
	now := s.now().Unix()
	return s.exec(ctx, `UPDATE devices SET last_seen_at = ?,
		prev_addr = CASE WHEN last_addr <> '' AND last_addr <> ? THEN last_addr ELSE prev_addr END,
		addr_changed_at = CASE WHEN last_addr <> '' AND last_addr <> ? THEN ? ELSE addr_changed_at END,
		last_addr = ?
		WHERE id = ? AND revoked_at = 0`, now, addr, addr, now, addr, id)
}

// DismissAddressChange clears a device's address change note. See
// hub.DeviceStore.
func (s *Store) DismissAddressChange(ctx context.Context, user, id string) error {
	return s.exec(ctx, `UPDATE devices SET addr_changed_at = 0 WHERE id = ? AND "user" = ?`, id, user)
}

// CodeFailure records a wrong pairing code. See hub.DeviceStore.
func (s *Store) CodeFailure(ctx context.Context, user string, window time.Duration) (int, error) {
	if err := s.exec(ctx, `INSERT INTO device_code_failures ("user", at) VALUES (?, ?)`, user, s.now().Unix()); err != nil {
		return 0, err
	}
	return s.CodeFailures(ctx, user, window)
}

// CodeFailures counts recent wrong pairing codes. See hub.DeviceStore.
func (s *Store) CodeFailures(ctx context.Context, user string, window time.Duration) (int, error) {
	var n int
	err := s.row(ctx, `SELECT COUNT(*) FROM device_code_failures WHERE "user" = ? AND at > ?`,
		user, s.now().Add(-window).Unix()).Scan(&n)
	return n, err
}
