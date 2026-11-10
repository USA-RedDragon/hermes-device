package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	user, groups, next, err := s.finishLogin(r)
	s.clearCookie(w, loginCookie)
	if err != nil {
		s.log.Warn("login callback", "error", err.Error())
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if s.groups != nil {
		if err := s.groups.SetGroups(r.Context(), user, groups); err != nil {
			s.log.Error("save groups", "user", user, "error", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	sess := session{User: user, Groups: groups, CSRF: randomToken(), Expires: time.Now().Add(s.ttl).Unix()}
	v, err := s.sessions.seal(sess)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, sessionCookie, v, s.ttl)
	s.log.Info("login", "user", user, "groups", groups)
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) finishLogin(r *http.Request) (user string, groups []string, next string, err error) {
	c, cerr := r.Cookie(s.cookieName(loginCookie))
	var l login
	if cerr != nil || s.logins.open(c.Value, &l) != nil || time.Now().Unix() >= l.Expires {
		return "", nil, "", fmt.Errorf("%w: missing or expired login cookie", errLogin)
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		return "", nil, "", fmt.Errorf("%w: provider error %q", errLogin, q.Get("error"))
	}
	if q.Get("state") == "" || q.Get("state") != l.State {
		return "", nil, "", fmt.Errorf("%w: state mismatch", errLogin)
	}
	provider, oc, err := s.oidc.get(r.Context())
	if err != nil {
		return "", nil, "", err
	}
	ctx := s.oidc.ctx(r.Context())
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(l.Verifier))
	if err != nil {
		return "", nil, "", fmt.Errorf("%w: code exchange: %w", errLogin, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return "", nil, "", fmt.Errorf("%w: no id_token", errLogin)
	}
	idt, err := provider.Verifier(&oidc.Config{ClientID: oc.ClientID}).Verify(ctx, raw)
	if err != nil {
		return "", nil, "", fmt.Errorf("%w: id_token: %w", errLogin, err)
	}
	if idt.Nonce != l.Nonce {
		return "", nil, "", fmt.Errorf("%w: nonce mismatch", errLogin)
	}
	cl := claims{}
	if err := idt.Claims(&cl); err != nil {
		return "", nil, "", fmt.Errorf("%w: claims: %w", errLogin, err)
	}
	if _, ok := cl[s.groupsClaim]; !ok || cl.str(s.usernameClaim) == "" {
		if info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			var extra claims
			if info.Claims(&extra) == nil && info.Subject == idt.Subject {
				for k, v := range extra {
					if _, ok := cl[k]; !ok {
						cl[k] = v
					}
				}
			}
		}
	}
	user = cl.str(s.usernameClaim)
	if user == "" {
		return "", nil, "", fmt.Errorf("%w: no %s claim", errLogin, s.usernameClaim)
	}
	return user, cl.list(s.groupsClaim), l.Next, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	s.clearCookie(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.session(r)
	writeJSON(w, http.StatusOK, map[string]any{"user": sess.User, "groups": sess.Groups, "csrf": sess.CSRF})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
