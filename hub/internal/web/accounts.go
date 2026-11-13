package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

// AccountStore keeps the hub's accounts. *store.Store implements it.
type AccountStore interface {
	CountAccounts(ctx context.Context) (int, error)
	CreateAccount(ctx context.Context, a store.Account) error
	CreateFirstAccount(ctx context.Context, a store.Account) error
	Account(ctx context.Context, name string) (store.Account, bool, error)
	AccountByIdentity(ctx context.Context, identity string) (store.Account, bool, error)
	AccountByEmail(ctx context.Context, email string) (store.Account, bool, error)
	Accounts(ctx context.Context) ([]store.Account, error)
	UpdateAccount(ctx context.Context, name string, u store.AccountUpdate) error
	DeleteAccount(ctx context.Context, name string) error
	LoginFailed(ctx context.Context, name string, maxFailures int, lockout time.Duration) (bool, error)
	LoginSucceeded(ctx context.Context, name string) error
}

// Login limits per client address, on top of each account's lockout.
const (
	loginsPerWindow = 10
	loginWindow     = time.Minute
	// accountCheckTTL is how long a replica trusts what it read about an
	// account for its sessions: a disable or new password ends the
	// account's sessions on other replicas within this time.
	accountCheckTTL = 5 * time.Second
)

// Messages of failed logins. One message for an unknown name, a wrong
// password and a locked account, so the answer does not tell which
// accounts exist.
const (
	msgBadLogin  = "Wrong name or password, or the account is locked for a while after too many tries"
	msgTooMany   = "Too many tries from this address. Wait a minute and try again"
	msgSetupDone = "The hub already has an account. Log in instead"
)

// accountState is what a replica last read about an account.
type accountState struct {
	epoch    int64
	disabled bool
	missing  bool
	at       time.Time
}

// accountCache keeps accountState for accountCheckTTL.
type accountCache struct {
	mu sync.Mutex
	m  map[string]accountState
}

func (c *accountCache) get(name string, now time.Time) (accountState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.m[name]
	if !ok || now.Sub(st.at) > accountCheckTTL {
		return accountState{}, false
	}
	return st, true
}

func (c *accountCache) put(name string, st accountState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 4096 {
		c.m = map[string]accountState{}
	}
	c.m[name] = st
}

func (c *accountCache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, name)
}

// accountOK reports whether a session of an account with epoch may still
// be used: the account exists, is not disabled and has not changed its
// password or had its sessions ended since.
func (s *Server) accountOK(ctx context.Context, name string, epoch int64) bool {
	if s.accounts == nil {
		return false
	}
	now := time.Now()
	st, ok := s.accountCache.get(name, now)
	if !ok {
		a, found, err := s.accounts.Account(ctx, name)
		if err != nil {
			s.log.Warn("read account", "user", name, "error", err.Error())
			return false
		}
		st = accountState{epoch: a.Epoch, disabled: a.Disabled, missing: !found, at: now}
		s.accountCache.put(name, st)
	}
	return !st.missing && !st.disabled && st.epoch == epoch
}

// startSession sets the session cookie of a login.
func (s *Server) startSession(w http.ResponseWriter, sess session) error {
	sess.CSRF = randomToken()
	sess.Expires = time.Now().Add(s.ttl).Unix()
	v, err := s.sessions.seal(sess)
	if err != nil {
		return err
	}
	s.setCookie(w, sessionCookie, v, s.ttl)
	return nil
}

func (s *Server) accountSession(a store.Account, identity string, groups []string) session {
	display := a.Display
	if display == "" {
		display = a.Name
	}
	return session{User: a.Name, Display: display, Groups: groups, Account: true, Epoch: a.Epoch, Identity: identity}
}

// clientAllowed counts a login or setup try from the request's address.
func (s *Server) clientAllowed(r *http.Request) bool {
	return s.limiter.Allow(auth.ClientIP(r, s.trusted).String())
}

// publicPost checks a POST that needs no session: same origin and JSON.
func (s *Server) publicPost(w http.ResponseWriter, r *http.Request, v any) bool {
	if !s.originOK(r) {
		s.crossOrigin(w)
		return false
	}
	return decode(w, r, v)
}

// needsSetup reports whether this is a local hub with no account yet, so
// the first visitor has to set it up.
func (s *Server) needsSetup(ctx context.Context) bool {
	if s.mode != config.AuthLocal || s.accounts == nil {
		return false
	}
	n, err := s.accounts.CountAccounts(ctx)
	return err == nil && n == 0
}

// handleAuthInfo tells the login and setup pages how this hub logs people
// in. It needs no session.
func (s *Server) handleAuthInfo(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{keyMode: s.mode, "setup": false, "logout_url": s.logoutURL}
	if s.mode == config.AuthLocal && s.accounts != nil {
		n, err := s.accounts.CountAccounts(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "internal error")
			return
		}
		out["setup"] = n == 0
	}
	writeJSON(w, http.StatusOK, out)
}

type passwordLogin struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Next     string `json:"next"`
}

// handlePasswordLogin logs in an account with its password (auth.mode
// local). Wrong passwords count toward the account's lockout and, with
// every other try, toward the client address's limit.
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if s.mode != config.AuthLocal || s.accounts == nil {
		errorJSON(w, http.StatusNotFound, "password login is off")
		return
	}
	var body passwordLogin
	if !s.publicPost(w, r, &body) {
		return
	}
	if !s.clientAllowed(r) {
		errorJSON(w, http.StatusTooManyRequests, msgTooMany)
		return
	}
	ctx := r.Context()
	name := strings.ToLower(strings.TrimSpace(body.Name))
	a, found, err := s.accounts.Account(ctx, name)
	if err != nil {
		s.log.Error("login", "error", err.Error())
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	hash := ""
	if found {
		hash = a.PasswordHash
	}
	ok, err := s.hasher.Verify(ctx, hash, body.Password)
	if err != nil {
		s.log.Error("login", "user", name, "error", err.Error())
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found || a.Disabled || a.Locked(time.Now()) || !ok {
		if found && !ok && !a.Locked(time.Now()) {
			locked, err := s.accounts.LoginFailed(ctx, a.Name, s.local.MaxFailures, s.local.Lockout)
			if err != nil {
				s.log.Error("count failed login", "user", a.Name, "error", err.Error())
			}
			if locked {
				s.log.Warn("account locked after wrong passwords", "user", a.Name, "for", s.local.Lockout)
			}
		}
		s.log.Info("login refused", "user", name, "client", auth.ClientIP(r, s.trusted).String())
		errorJSON(w, http.StatusUnauthorized, msgBadLogin)
		return
	}
	if a.Failures > 0 {
		if err := s.accounts.LoginSucceeded(ctx, a.Name); err != nil {
			s.log.Warn("clear failed logins", "user", a.Name, "error", err.Error())
		}
	}
	if err := s.startSession(w, s.accountSession(a, "", nil)); err != nil {
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("login", "user", a.Name, "mode", s.mode)
	writeJSON(w, http.StatusOK, map[string]string{"next": safeNext(body.Next)})
}

type setupBody struct {
	Name     string `json:"name"`
	Display  string `json:"display"`
	Password string `json:"password"`
}

// handleSetup makes the first account, an admin, while the hub has none
// (auth.mode local). It takes no token: the first visitor of the setup page
// becomes the admin, as in DMRHub, and the hub warns about that at start.
// Two replicas cannot both make one, because CreateFirstAccount decides in
// the database.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.mode != config.AuthLocal || s.accounts == nil {
		errorJSON(w, http.StatusNotFound, "setup is off")
		return
	}
	var body setupBody
	if !s.publicPost(w, r, &body) {
		return
	}
	if !s.clientAllowed(r) {
		errorJSON(w, http.StatusTooManyRequests, msgTooMany)
		return
	}
	ctx := r.Context()
	if n, err := s.accounts.CountAccounts(ctx); err != nil || n > 0 {
		errorJSON(w, http.StatusConflict, msgSetupDone)
		return
	}
	a, msg := s.newLocalAccount(ctx, body.Name, body.Display, "", body.Password)
	if msg != "" {
		errorJSON(w, http.StatusBadRequest, msg)
		return
	}
	a.Admin = true
	a.Backends = s.backendIDs
	switch err := s.accounts.CreateFirstAccount(ctx, a); {
	case errors.Is(err, store.ErrNotFirstAccount):
		errorJSON(w, http.StatusConflict, msgSetupDone)
		return
	case err != nil:
		s.log.Error("setup", "error", err.Error())
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("first account made", "user", a.Name)
	if err := s.startSession(w, s.accountSession(a, "", nil)); err != nil {
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"user": a.Name, "next": "/"})
}

// newLocalAccount checks a new account's name, display name and password
// and hashes the password. A bad value gives a message for the user. An
// empty password is allowed only outside auth.mode local, where people
// log in through the proxy or OIDC.
func (s *Server) newLocalAccount(ctx context.Context, name, display, email, password string) (store.Account, string) {
	name = strings.ToLower(strings.TrimSpace(name))
	display, email = strings.TrimSpace(display), strings.TrimSpace(email)
	switch {
	case !auth.ValidName(name):
		return store.Account{}, "A name has 1 to 64 lowercase letters, digits, dots, dashes or underscores, and starts with a letter or digit"
	case s.configNames[name]:
		return store.Account{}, "users[] in the hub's config already has that name"
	case len(display) > 100 || len(email) > 254:
		return store.Account{}, "The display name or email is too long"
	case email != "" && !strings.Contains(email, "@"):
		return store.Account{}, "That is not an email address"
	}
	a := store.Account{Name: name, Display: display, Email: email}
	if password == "" && s.mode != config.AuthLocal {
		return a, ""
	}
	if err := auth.CheckPassword(password, name); err != nil {
		return store.Account{}, capitalize(err.Error())
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return store.Account{}, "Could not save the password. Try again"
	}
	a.PasswordHash = hash
	return a, ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// handleChangePassword sets a new password for the session's own account
// after checking the current one. It ends the account's other sessions
// and gives this browser a new one.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.session(r)
	if s.mode != config.AuthLocal || sess == nil || !sess.Account {
		errorJSON(w, http.StatusNotFound, "this login has no password here")
		return
	}
	var body struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.clientAllowed(r) {
		errorJSON(w, http.StatusTooManyRequests, msgTooMany)
		return
	}
	ctx := r.Context()
	a, found, err := s.accounts.Account(ctx, sess.User)
	if err != nil || !found {
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	if ok, err := s.hasher.Verify(ctx, a.PasswordHash, body.Current); err != nil || !ok {
		if _, err := s.accounts.LoginFailed(ctx, a.Name, s.local.MaxFailures, s.local.Lockout); err != nil {
			s.log.Warn("count failed login", "user", a.Name, "error", err.Error())
		}
		errorJSON(w, http.StatusForbidden, "The current password is not right")
		return
	}
	if err := auth.CheckPassword(body.Password, a.Name); err != nil {
		errorJSON(w, http.StatusBadRequest, capitalize(err.Error()))
		return
	}
	hash, err := s.hasher.Hash(ctx, body.Password)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.accounts.UpdateAccount(ctx, a.Name, store.AccountUpdate{PasswordHash: &hash}); err != nil {
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.accountCache.forget(a.Name)
	a.Epoch++
	next := s.accountSession(a, "", sess.Groups)
	next.Display = sess.Display
	if err := s.startSession(w, next); err != nil {
		errorJSON(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("password changed", "user", a.Name)
	w.WriteHeader(http.StatusNoContent)
}

// resolve finds or makes the account of an identity from the OIDC
// provider (kind "oidc") or proxy (kind "proxy"). It returns the session
// to start, or false when the identity may not log in.
func (s *Server) resolve(ctx context.Context, kind string, id auth.Identity, autoCreate bool) (session, bool, error) {
	if user, ok := s.users[id.ID]; ok {
		return session{User: user, Display: id.Display, Groups: id.Groups, Identity: id.ID}, true, nil
	}
	if s.accounts == nil {
		return session{}, false, nil
	}
	key := kind + ":" + id.ID
	a, found, err := s.accounts.AccountByIdentity(ctx, key)
	if err != nil {
		return session{}, false, err
	}
	if !found {
		a, found, err = s.bindPrepared(ctx, kind, id, key)
		if err != nil {
			return session{}, false, err
		}
	}
	if !found && autoCreate && s.mayAutoCreate(kind, id) {
		a, found, err = s.autoCreate(ctx, kind, id, key)
		if err != nil {
			return session{}, false, err
		}
	}
	if !found || a.Disabled {
		return session{}, false, nil
	}
	sess := s.accountSession(a, id.ID, id.Groups)
	if id.Display != "" {
		sess.Display = id.Display
	}
	return sess, true, nil
}

// bindPrepared binds an identity to an account an admin made for it: by
// verified email, or, for a proxy, by an account name equal to the
// identity.
func (s *Server) bindPrepared(ctx context.Context, kind string, id auth.Identity, key string) (store.Account, bool, error) {
	var a store.Account
	var found bool
	var err error
	if id.Email != "" && id.EmailVerified {
		if a, found, err = s.accounts.AccountByEmail(ctx, id.Email); err != nil {
			return a, false, err
		}
	}
	if !found && kind == config.AuthProxy && auth.ValidName(id.ID) {
		if a, found, err = s.accounts.Account(ctx, id.ID); err != nil {
			return a, false, err
		}
		found = found && a.Identity == "" && a.PasswordHash == ""
	}
	if !found {
		return a, false, nil
	}
	if err := s.accounts.UpdateAccount(ctx, a.Name, store.AccountUpdate{Identity: &key}); err != nil {
		return a, false, err
	}
	s.log.Info("login bound to account", "user", a.Name, "identity", key)
	a.Identity = key
	return a, true, nil
}

func (s *Server) mayAutoCreate(kind string, id auth.Identity) bool {
	if kind != config.AuthOIDC {
		return true
	}
	if slices.ContainsFunc(id.Groups, func(g string) bool { return slices.Contains(s.oidcGroups, g) }) {
		return true
	}
	if !id.EmailVerified || id.Email == "" {
		return false
	}
	_, domain, _ := strings.Cut(strings.ToLower(id.Email), "@")
	return slices.ContainsFunc(s.oidcDomains, func(d string) bool { return d == "*" || strings.EqualFold(d, domain) })
}

// autoCreate makes an account for a new identity. Its name comes from the proxy identity, or from
// the display name or email of an OIDC login, whose sub is opaque. The first account the
// hub makes this way is an admin when auth.admins names nobody.
func (s *Server) autoCreate(ctx context.Context, kind string, id auth.Identity, key string) (store.Account, bool, error) {
	base := auth.NameFor(id.ID, id.Email, id.Display)
	if kind == config.AuthOIDC {
		base = auth.NameFor(id.Display, id.Email, id.ID)
	}
	n, err := s.accounts.CountAccounts(ctx)
	if err != nil {
		return store.Account{}, false, err
	}
	for i := 1; i <= 50; i++ {
		name := auth.Numbered(base, i)
		if s.configNames[name] {
			continue
		}
		a := store.Account{Name: name, Display: id.Display, Email: id.Email, Identity: key, Admin: n == 0 && len(s.admins) == 0}
		err := s.accounts.CreateAccount(ctx, a)
		if errors.Is(err, store.ErrAccountExists) {
			if b, found, err := s.accounts.AccountByIdentity(ctx, key); err != nil || found {
				return b, found, err
			}
			continue
		}
		if err != nil {
			return store.Account{}, false, err
		}
		s.log.Info("account made at first login", "user", name, "identity", key, "admin", a.Admin)
		return a, true, nil
	}
	return store.Account{}, false, fmt.Errorf("no free account name for %s", key)
}
