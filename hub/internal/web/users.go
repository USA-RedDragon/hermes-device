package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

// isAdmin reports whether user may manage people: auth.admins names them,
// or their account is an admin and not disabled.
func (s *Server) isAdmin(ctx context.Context, user string) bool {
	if s.admins[user] {
		return true
	}
	if s.accounts == nil {
		return false
	}
	a, found, err := s.accounts.Account(ctx, user)
	return err == nil && found && a.Admin && !a.Disabled
}

// adminAPI is api for admins only.
func (s *Server) adminAPI(h http.HandlerFunc) http.Handler {
	return s.api(func(w http.ResponseWriter, r *http.Request) {
		if s.accounts == nil || !s.isAdmin(r.Context(), userFrom(r)) {
			errorJSON(w, http.StatusForbidden, "only an admin may manage people")
			return
		}
		h(w, r)
	})
}

func (s *Server) userRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/info", s.handleAuthInfo)
	mux.HandleFunc("POST /auth/password", s.handlePasswordLogin)
	mux.HandleFunc("POST /auth/setup", s.handleSetup)
	mux.Handle("POST /api/account/password", s.api(s.handleChangePassword))
	mux.Handle("GET /api/admin/users", s.adminAPI(s.handleUsers))
	mux.Handle("POST /api/admin/users", s.adminAPI(s.handleAddUser))
	mux.Handle("POST /api/admin/users/{name}", s.adminAPI(s.handleEditUser))
	mux.Handle("POST /api/admin/users/{name}/password", s.adminAPI(s.handleResetPassword))
	mux.Handle("POST /api/admin/users/{name}/delete", s.adminAPI(s.handleDeleteUser))
}

type userView struct {
	Name     string `json:"name"`
	Display  string `json:"display"`
	Email    string `json:"email"`
	Identity string `json:"identity"`
	Admin    bool   `json:"admin"`
	Disabled bool   `json:"disabled"`
	Locked   bool   `json:"locked"`
	// LockedUntil is when a locked account unlocks, in Unix seconds.
	LockedUntil int64 `json:"locked_until,omitempty"`
	// Failures counts the wrong passwords since the last good login.
	Failures int      `json:"failures,omitempty"`
	Password bool     `json:"password"`
	Backends []string `json:"backends"`
	// Groups are the groups of the person's latest login, which the
	// backends' groups match.
	Groups []string `json:"groups"`
	// Source is "account" for an account in the hub's database and
	// "config" for an entry of users[], which only the config changes.
	Source  string `json:"source"`
	Created int64  `json:"created,omitempty"`
}

type backendView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Everyone bool     `json:"everyone"`
	Users    []string `json:"users"`
	Groups   []string `json:"groups"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.accounts.Accounts(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now()
	users := make([]userView, 0, len(accounts)+len(s.users))
	for _, a := range accounts {
		users = append(users, userView{Groups: s.groupsOf(r.Context(), a.Name), Name: a.Name, Display: a.Display, Email: a.Email, Identity: a.Identity,
			Admin: a.Admin || s.admins[a.Name], Disabled: a.Disabled, Locked: a.Locked(now), LockedUntil: lockedUntil(a, now), Failures: a.Failures, Password: a.PasswordHash != "",
			Backends: nonNil(a.Backends), Source: "account", Created: a.CreatedAt.Unix()})
	}
	for sub, name := range s.users {
		users = append(users, userView{Name: name, Identity: s.mode + ":" + sub, Admin: s.admins[name], Backends: []string{}, Groups: s.groupsOf(r.Context(), name), Source: "config"})
	}
	slices.SortFunc(users, func(a, b userView) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string]any{keyMode: s.mode, "users": users, "backends": s.backendViews, "me": userFrom(r), "login": s.loginInfo})
}

// groupsOf is the groups of a person's latest login, or none.
func (s *Server) groupsOf(ctx context.Context, name string) []string {
	g, ok := s.groups.(interface {
		Groups(ctx context.Context, user string) ([]string, error)
	})
	if !ok {
		return []string{}
	}
	list, err := g.Groups(ctx, name)
	if err != nil {
		return []string{}
	}
	return nonNil(list)
}

func lockedUntil(a store.Account, now time.Time) int64 {
	if !a.Locked(now) {
		return 0
	}
	return a.LockedUntil.Unix()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// checkBackends keeps only known backend ids and reports an unknown one.
func (s *Server) checkBackends(ids []string) ([]string, bool) {
	out := []string{}
	for _, id := range ids {
		if !slices.Contains(s.backendIDs, id) {
			return nil, false
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, true
}

type userBody struct {
	Name     string    `json:"name"`
	Display  *string   `json:"display"`
	Email    *string   `json:"email"`
	Password string    `json:"password"`
	Admin    *bool     `json:"admin"`
	Disabled *bool     `json:"disabled"`
	Backends *[]string `json:"backends"`
}

func (s *Server) handleAddUser(w http.ResponseWriter, r *http.Request) {
	var body userBody
	if !decode(w, r, &body) {
		return
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	ctx := r.Context()
	a, msg := s.newLocalAccount(ctx, body.Name, deref(body.Display), deref(body.Email), body.Password)
	if msg != "" {
		errorJSON(w, http.StatusBadRequest, msg)
		return
	}
	a.Admin = body.Admin != nil && *body.Admin
	if body.Backends != nil {
		ids, ok := s.checkBackends(*body.Backends)
		if !ok {
			errorJSON(w, http.StatusBadRequest, "unknown backend")
			return
		}
		a.Backends = ids
	}
	switch err := s.accounts.CreateAccount(ctx, a); {
	case errors.Is(err, store.ErrAccountExists):
		errorJSON(w, http.StatusConflict, "An account with that name already exists")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	s.log.Info("account added", "user", a.Name, "by", userFrom(r), "admin", a.Admin)
	writeJSON(w, http.StatusCreated, map[string]string{"name": a.Name})
}

// activeAdmins counts the admin accounts that are not disabled, besides
// skip.
func (s *Server) activeAdmins(ctx context.Context, skip string) (int, error) {
	accounts, err := s.accounts.Accounts(ctx)
	if err != nil {
		return 0, err
	}
	n := len(s.admins)
	for _, a := range accounts {
		if a.Name != skip && a.Admin && !a.Disabled && !s.admins[a.Name] {
			n++
		}
	}
	return n, nil
}

func (s *Server) handleEditUser(w http.ResponseWriter, r *http.Request) {
	var body userBody
	if !decode(w, r, &body) {
		return
	}
	ctx, name, me := r.Context(), r.PathValue("name"), userFrom(r)
	a, found, err := s.accounts.Account(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		errorJSON(w, http.StatusNotFound, "no such account")
		return
	}
	u := store.AccountUpdate{Admin: body.Admin, Disabled: body.Disabled}
	if body.Display != nil {
		d := strings.TrimSpace(*body.Display)
		if len(d) > 100 {
			errorJSON(w, http.StatusBadRequest, "The display name is too long")
			return
		}
		u.Display = &d
	}
	if body.Email != nil {
		e := strings.TrimSpace(*body.Email)
		if len(e) > 254 || e != "" && !strings.Contains(e, "@") {
			errorJSON(w, http.StatusBadRequest, "That is not an email address")
			return
		}
		u.Email = &e
	}
	if body.Backends != nil {
		ids, ok := s.checkBackends(*body.Backends)
		if !ok {
			errorJSON(w, http.StatusBadRequest, "unknown backend")
			return
		}
		u.Backends = &ids
	}
	losesAdmin := a.Admin && (body.Admin != nil && !*body.Admin || body.Disabled != nil && *body.Disabled)
	if name == me && losesAdmin {
		errorJSON(w, http.StatusBadRequest, "You cannot take away your own admin rights or disable yourself")
		return
	}
	if losesAdmin {
		n, err := s.activeAdmins(ctx, name)
		if err != nil {
			s.fail(w, err)
			return
		}
		if n == 0 {
			errorJSON(w, http.StatusBadRequest, "The hub needs at least one admin")
			return
		}
	}
	if err := s.accounts.UpdateAccount(ctx, name, u); err != nil {
		s.fail(w, err)
		return
	}
	s.accountCache.forget(name)
	s.log.Info("account changed", "user", name, "by", me)
	w.WriteHeader(http.StatusNoContent)
}

// handleResetPassword sets an account's password, unlocks it and ends its
// sessions (auth.mode local).
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if s.mode != config.AuthLocal {
		errorJSON(w, http.StatusNotFound, "passwords are only used in auth.mode local")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	ctx, name := r.Context(), r.PathValue("name")
	if err := auth.CheckPassword(body.Password, name); err != nil {
		errorJSON(w, http.StatusBadRequest, capitalize(err.Error()))
		return
	}
	hash, err := s.hasher.Hash(ctx, body.Password)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch err := s.accounts.UpdateAccount(ctx, name, store.AccountUpdate{PasswordHash: &hash}); {
	case errors.Is(err, store.ErrNoAccount):
		errorJSON(w, http.StatusNotFound, "no such account")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	s.accountCache.forget(name)
	s.log.Info("password set by admin", "user", name, "by", userFrom(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	ctx, name, me := r.Context(), r.PathValue("name"), userFrom(r)
	if name == me {
		errorJSON(w, http.StatusBadRequest, "You cannot delete your own account")
		return
	}
	a, found, err := s.accounts.Account(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		errorJSON(w, http.StatusNotFound, "no such account")
		return
	}
	if a.Admin {
		n, err := s.activeAdmins(ctx, name)
		if err != nil {
			s.fail(w, err)
			return
		}
		if n == 0 {
			errorJSON(w, http.StatusBadRequest, "The hub needs at least one admin")
			return
		}
	}
	if err := s.accounts.DeleteAccount(ctx, name); err != nil {
		s.fail(w, err)
		return
	}
	s.accountCache.forget(name)
	s.log.Info("account deleted", "user", name, "by", me)
	w.WriteHeader(http.StatusNoContent)
}
