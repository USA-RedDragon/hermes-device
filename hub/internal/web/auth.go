package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "hub_session"
	loginCookie   = "hub_login"
)

// oidcClient discovers the provider on first use, so the hub starts even
// when the issuer is down.
type oidcClient struct {
	issuer, clientID, clientSecret, redirect string
	scopes                                   []string
	httpClient                               *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

func (o *oidcClient) ctx(ctx context.Context) context.Context {
	if o.httpClient != nil {
		return oidc.ClientContext(ctx, o.httpClient)
	}
	return ctx
}

func (o *oidcClient) get(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		p, err := oidc.NewProvider(o.ctx(ctx), o.issuer)
		if err != nil {
			return nil, nil, fmt.Errorf("oidc discovery: %w", err)
		}
		o.provider = p
	}
	return o.provider, &oauth2.Config{
		ClientID: o.clientID, ClientSecret: o.clientSecret, RedirectURL: o.redirect,
		Endpoint: o.provider.Endpoint(), Scopes: o.scopes,
	}, nil
}

// safeNext keeps only same-origin absolute paths.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

// handleLogin starts a login: the OIDC redirect, the proxy's identity, or
// the hub's own login page.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch s.mode {
	case config.AuthLocal:
		http.Redirect(w, r, "/login?next="+url.QueryEscape(safeNext(r.URL.Query().Get("next"))), http.StatusFound)
		return
	case config.AuthProxy:
		s.handleProxyLogin(w, r)
		return
	}
	_, oc, err := s.oidc.get(r.Context())
	if err != nil {
		s.log.Error("login", "error", err.Error())
		http.Error(w, "login provider unavailable", http.StatusBadGateway)
		return
	}
	l := login{State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier(),
		Next: safeNext(r.URL.Query().Get("next")), Expires: time.Now().Add(loginTTL).Unix()}
	v, err := s.logins.seal(l)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, loginCookie, v, loginTTL)
	http.Redirect(w, r, oc.AuthCodeURL(l.State, oidc.Nonce(l.Nonce), oauth2.S256ChallengeOption(l.Verifier)), http.StatusFound)
}

type claims map[string]any

func (c claims) str(name string) string {
	s, _ := c[name].(string)
	return s
}

func (c claims) list(name string) []string {
	switch v := c[name].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}

var errLogin = errors.New("login failed")

// identity is what a verified login says about the user.
type identity struct {
	sub, display, next, email string
	emailVerified             bool
	groups                    []string
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.mode != config.AuthOIDC {
		http.NotFound(w, r)
		return
	}
	id, err := s.finishLogin(r)
	s.clearCookie(w, loginCookie)
	if err != nil {
		s.log.Warn("login callback", "error", err.Error())
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	ident := auth.Identity{ID: id.sub, Display: id.display, Email: id.email, EmailVerified: id.emailVerified, Groups: id.groups}
	if !s.finishIdentity(w, r, config.AuthOIDC, ident, s.oidcAuto) {
		return
	}
	http.Redirect(w, r, id.next, http.StatusFound)
}

// finishIdentity starts the session of an identity from the OIDC provider
// or the proxy, or answers that it may not log in.
func (s *Server) finishIdentity(w http.ResponseWriter, r *http.Request, kind string, id auth.Identity, autoCreate bool) bool {
	sess, ok, err := s.resolve(r.Context(), kind, id, autoCreate)
	if err != nil {
		s.log.Error("login", "identity", id.ID, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !ok {
		s.log.Warn("login refused: no account for this identity", "mode", kind, "identity", id.ID, "display", id.Display)
		s.renderRefused(w, r, id)
		return false
	}
	if s.groups != nil {
		if err := s.groups.SetGroups(r.Context(), sess.User, id.Groups); err != nil {
			s.log.Error("save groups", "user", sess.User, "error", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return false
		}
	}
	if err := s.startSession(w, sess); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	s.log.Info("login", "user", sess.User, "mode", kind, "identity", id.ID, "display", id.Display, "groups", id.Groups)
	return true
}

// handleProxyLogin starts a session for the identity the proxy sends
// (auth.mode proxy).
func (s *Server) handleProxyLogin(w http.ResponseWriter, r *http.Request) {
	id, err := s.proxy.Identity(r)
	if err != nil {
		s.log.Warn("proxy login", "client", auth.Peer(r).String(), "error", err.Error())
		s.renderProxyError(w, r)
		return
	}
	if !s.finishIdentity(w, r, config.AuthProxy, id, s.proxyAuto) {
		return
	}
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound) //nolint:gosec // safeNext keeps only same-origin paths
}

// finishLogin checks the callback against the login cookie, exchanges the
// code with the PKCE verifier and verifies the ID token and its nonce.
func (s *Server) finishLogin(r *http.Request) (identity, error) {
	fail := func(format string, a ...any) (identity, error) {
		return identity{}, fmt.Errorf("%w: "+format, append([]any{errLogin}, a...)...)
	}
	c, cerr := r.Cookie(s.cookieName(loginCookie))
	var l login
	if cerr != nil || s.logins.open(c.Value, &l) != nil || time.Now().Unix() >= l.Expires {
		return fail("missing or expired login cookie")
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		return fail("provider error %q", q.Get("error"))
	}
	if q.Get("state") == "" || q.Get("state") != l.State {
		return fail("state mismatch")
	}
	provider, oc, err := s.oidc.get(r.Context())
	if err != nil {
		return identity{}, err
	}
	ctx := s.oidc.ctx(r.Context())
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(l.Verifier))
	if err != nil {
		return fail("code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return fail("no id_token")
	}
	idt, err := provider.Verifier(&oidc.Config{ClientID: oc.ClientID}).Verify(ctx, raw)
	if err != nil {
		return fail("id_token: %w", err)
	}
	if idt.Nonce != l.Nonce {
		return fail("nonce mismatch")
	}
	if idt.Subject == "" {
		return fail("no sub claim")
	}
	cl := claims{}
	if err := idt.Claims(&cl); err != nil {
		return fail("claims: %w", err)
	}
	if _, ok := cl[s.groupsClaim]; !ok || cl.str(s.displayClaim) == "" {
		if info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil && info.Subject == idt.Subject {
			var extra claims
			if info.Claims(&extra) == nil {
				for k, v := range extra {
					if _, ok := cl[k]; !ok {
						cl[k] = v
					}
				}
			}
		}
	}
	verified, _ := cl["email_verified"].(bool)
	return identity{sub: idt.Subject, display: cl.str(s.displayClaim), groups: cl.list(s.groupsClaim), next: l.Next,
		email: cl.str(s.emailClaim), emailVerified: verified}, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	s.clearCookie(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.session(r)
	modelBackends := s.modelAdminBackends(r.Context(), sess.User)
	writeJSON(w, http.StatusOK, map[string]any{"user": sess.User, "display": sess.Display, "groups": sess.Groups, "csrf": sess.CSRF,
		"default_backend": s.defaults[sess.User], "devices": s.devices != nil, "computers": s.computers != nil,
		"memory": s.memory != nil, "usage": s.insights != nil || s.claudeCode != nil, "claude_code": s.claudeCode != nil,
		"files": s.files != nil, "notify": s.notices != nil,
		"claude": s.hasClaude(r.Context(), sess.User), "claude_admin": s.isClaudeAdmin(sess.User), "telegram_steps": s.telegramSteps, "version": s.version,
		"scheduled": s.scheduled != nil, "prompts": s.prompts != nil, "partners": s.partners != nil,
		"auth": s.mode, "admin": s.accounts != nil && s.isAdmin(r.Context(), sess.User), "password": s.mode == config.AuthLocal && sess.Account,
		"models_admin": len(modelBackends) > 0, "models_admin_backends": modelBackends,
		"logout_url": s.logoutURL, "identity": sess.Identity, "issuer": s.issuer})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write JSON answer", "error", err.Error())
	}
}

func errorJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
