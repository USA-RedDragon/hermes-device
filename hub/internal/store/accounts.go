package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Account errors.
var (
	ErrAccountExists   = errors.New("store: an account with that name or identity exists")
	ErrNoAccount       = errors.New("store: no such account")
	ErrNotFirstAccount = errors.New("store: the hub already has an account")
)

// Account is a person who logs in to the hub: with a password (auth.mode
// local), or by an identity from the proxy or OIDC provider.
type Account struct {
	Name    string
	Display string
	Email   string
	// Identity is the outside identity bound to the account, as
	// "oidc:<sub>" or "proxy:<identity>". Empty for a local account and
	// for one an admin made that has not logged in yet.
	Identity     string
	PasswordHash string
	Admin        bool
	Disabled     bool
	Failures     int
	LockedUntil  time.Time
	// Epoch goes up with each password change, disable and session
	// revoke; a session cookie of an older epoch is refused.
	Epoch     int64
	Backends  []string
	CreatedAt time.Time
}

// Locked reports whether the account is locked at now.
func (a Account) Locked(now time.Time) bool { return now.Before(a.LockedUntil) }

// accountLockKey is the PostgreSQL advisory lock that makes the first
// account exactly once across replicas.
const accountLockKey = 0x6875622d66697273 // "hub-firs"

const accountColumns = `name, display, email, identity, password_hash, admin, disabled, failures, locked_until, epoch, created_at`

func scanAccount(row interface{ Scan(dest ...any) error }) (Account, error) {
	var a Account
	var admin, disabled int
	var locked, created int64
	err := row.Scan(&a.Name, &a.Display, &a.Email, &a.Identity, &a.PasswordHash, &admin, &disabled, &a.Failures, &locked, &a.Epoch, &created)
	a.Admin, a.Disabled = admin != 0, disabled != 0
	if locked > 0 {
		a.LockedUntil = time.Unix(locked, 0)
	}
	a.CreatedAt = time.Unix(created, 0)
	return a, err
}

// CountAccounts returns how many accounts the hub has.
func (s *Store) CountAccounts(ctx context.Context) (int, error) {
	var n int
	err := s.row(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&n)
	return n, err
}

// CreateAccount adds an account with its backends.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return s.insertAccount(ctx, tx, a) })
}

// CreateFirstAccount adds an account only while the hub has none, also
// when several replicas try at once. It fails with ErrNotFirstAccount
// otherwise.
func (s *Store) CreateFirstAccount(ctx context.Context, a Account) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if s.dialect == postgresDialect {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(accountLockKey)); err != nil {
				return err
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrNotFirstAccount
		}
		return s.insertAccount(ctx, tx, a)
	})
}

func (s *Store) insertAccount(ctx context.Context, tx *sql.Tx, a Account) error {
	var exists int
	err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM accounts WHERE name = ? OR (identity <> '' AND identity = ?)`),
		a.Name, a.Identity).Scan(&exists)
	if err != nil {
		return err
	}
	if exists > 0 {
		return ErrAccountExists
	}
	now := s.now().Unix()
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO accounts (name, display, email, identity, password_hash, admin, disabled,
		failures, locked_until, epoch, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?)`),
		a.Name, a.Display, a.Email, a.Identity, a.PasswordHash, boolInt(a.Admin), boolInt(a.Disabled), now, now); err != nil {
		return err
	}
	return setGrants(ctx, s, tx, a.Name, a.Backends)
}

func setGrants(ctx context.Context, s *Store, tx *sql.Tx, name string, backends []string) error {
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM account_backends WHERE name = ?`), name); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range backends {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO account_backends (name, backend_id) VALUES (?, ?)`), name, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) accountWhere(ctx context.Context, where string, arg any) (Account, bool, error) {
	a, err := scanAccount(s.row(ctx, `SELECT `+accountColumns+` FROM accounts WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	if a.Backends, err = s.Grants(ctx, a.Name); err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// Account returns the account with that name.
func (s *Store) Account(ctx context.Context, name string) (Account, bool, error) {
	return s.accountWhere(ctx, `name = ?`, name)
}

// AccountByIdentity returns the account bound to an outside identity.
func (s *Store) AccountByIdentity(ctx context.Context, identity string) (Account, bool, error) {
	if identity == "" {
		return Account{}, false, nil
	}
	return s.accountWhere(ctx, `identity = ?`, identity)
}

// AccountByEmail returns the account with that email that no outside
// identity is bound to yet: one an admin made for a person who has not
// logged in.
func (s *Store) AccountByEmail(ctx context.Context, email string) (Account, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return Account{}, false, nil
	}
	return s.accountWhere(ctx, `identity = '' AND password_hash = '' AND lower(email) = ?`, email)
}

// Accounts lists every account by name.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	grants, err := s.allGrants(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Backends = grants[out[i].Name]
	}
	return out, rows.Err()
}

func (s *Store) allGrants(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, backend_id FROM account_backends ORDER BY name, backend_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		out[name] = append(out[name], id)
	}
	return out, rows.Err()
}

// AccountNames lists the names of every account.
func (s *Store) AccountNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Grants returns the backends an admin gave the account.
func (s *Store) Grants(ctx context.Context, name string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT backend_id FROM account_backends WHERE name = ? ORDER BY backend_id`), name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AccountUpdate is a change to an account. Nil fields stay as they are.
type AccountUpdate struct {
	Display  *string
	Email    *string
	Admin    *bool
	Disabled *bool
	Backends *[]string
	// PasswordHash sets a new password hash, unlocks the account and ends
	// its sessions.
	PasswordHash *string
	// Identity binds an outside identity.
	Identity *string
	// Revoke ends the account's sessions.
	Revoke bool
}

// UpdateAccount applies u to the account. Disabling an account, a new
// password and Revoke end its sessions.
func (s *Store) UpdateAccount(ctx context.Context, name string, u AccountUpdate) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM accounts WHERE name = ?`), name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNoAccount
		}
		set, args := []string{"updated_at = ?"}, []any{s.now().Unix()}
		add := func(col string, v any) {
			set = append(set, col+" = ?")
			args = append(args, v)
		}
		if u.Display != nil {
			add("display", *u.Display)
		}
		if u.Email != nil {
			add("email", *u.Email)
		}
		if u.Admin != nil {
			add("admin", boolInt(*u.Admin))
		}
		if u.Identity != nil {
			add("identity", *u.Identity)
		}
		revoke := u.Revoke
		if u.Disabled != nil {
			add("disabled", boolInt(*u.Disabled))
			revoke = revoke || *u.Disabled
		}
		if u.PasswordHash != nil {
			add("password_hash", *u.PasswordHash)
			add("failures", 0)
			add("locked_until", 0)
			revoke = true
		}
		if revoke {
			set = append(set, "epoch = epoch + 1")
		}
		args = append(args, name)
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE accounts SET `+strings.Join(set, ", ")+` WHERE name = ?`), args...); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrAccountExists
			}
			return err
		}
		if u.Backends != nil {
			return setGrants(ctx, s, tx, name, *u.Backends)
		}
		return nil
	})
}

// DeleteAccount removes the account and its backends.
func (s *Store) DeleteAccount(ctx context.Context, name string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM accounts WHERE name = ?`), name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoAccount
		}
		_, err = tx.ExecContext(ctx, s.q(`DELETE FROM account_backends WHERE name = ?`), name)
		return err
	})
}

// LoginFailed counts a wrong password. After max in a row the account is
// locked for lockout, and the count starts again. It reports whether the
// account is locked now.
func (s *Store) LoginFailed(ctx context.Context, name string, maxFailures int, lockout time.Duration) (bool, error) {
	var locked bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var failures int
		err := tx.QueryRowContext(ctx, s.q(`SELECT failures FROM accounts WHERE name = ?`), name).Scan(&failures)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		failures++
		until := int64(0)
		if failures >= maxFailures {
			failures, until, locked = 0, s.now().Add(lockout).Unix(), true
		}
		if until > 0 {
			_, err = tx.ExecContext(ctx, s.q(`UPDATE accounts SET failures = ?, locked_until = ? WHERE name = ?`), failures, until, name)
		} else {
			_, err = tx.ExecContext(ctx, s.q(`UPDATE accounts SET failures = ? WHERE name = ?`), failures, name)
		}
		return err
	})
	return locked, err
}

// LoginSucceeded clears the account's count of wrong passwords.
func (s *Store) LoginSucceeded(ctx context.Context, name string) error {
	return s.exec(ctx, `UPDATE accounts SET failures = 0 WHERE name = ? AND failures <> 0`, name)
}

// Meta keys of auth secrets.
const (
	metaSessionSecret = "session_secret"
	metaSetupToken    = "setup_token_hash"
)

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SessionSecret returns the session key the hub made and keeps in the
// database for installs that set no session.secret. Every replica gets
// the same one.
func (s *Store) SessionSecret(ctx context.Context) (string, error) {
	fresh, err := randomSecret()
	if err != nil {
		return "", err
	}
	if err := s.exec(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, metaSessionSecret, fresh); err != nil {
		return "", err
	}
	var v string
	err = s.row(ctx, `SELECT value FROM meta WHERE key = ?`, metaSessionSecret).Scan(&v)
	return v, err
}

// NewSetupToken makes a first-user setup token, keeps only its hash and
// returns it. A new token replaces the one before.
func (s *Store) NewSetupToken(ctx context.Context) (string, error) {
	tok, err := randomSecret()
	if err != nil {
		return "", err
	}
	err = s.exec(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaSetupToken, tokenHash(tok))
	return tok, err
}

// SetupTokenOK reports whether tok is the stored setup token.
func (s *Store) SetupTokenOK(ctx context.Context, tok string) (bool, error) {
	var v string
	err := s.row(ctx, `SELECT value FROM meta WHERE key = ?`, metaSetupToken).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || tok == "" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(v), []byte(tokenHash(tok))) == 1, nil
}

// DropSetupToken forgets the setup token once the first account exists.
func (s *Store) DropSetupToken(ctx context.Context) error {
	return s.exec(ctx, `DELETE FROM meta WHERE key = ?`, metaSetupToken)
}
