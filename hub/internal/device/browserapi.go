package device

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const codeInvalidGrant = "invalid_grant"

var codeVerifierRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// BrowserHandler serves the browser sign-in API that the Hermes Firefox
// extension calls: POST /api/browser/token and POST /api/browser/revoke.
func (s *Server) BrowserHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/browser/token", s.handleBrowserToken)
	mux.HandleFunc("POST /api/browser/revoke", s.handleBrowserRevoke)
	mux.HandleFunc("/api/browser/", func(w http.ResponseWriter, _ *http.Request) {
		apiError(w, http.StatusNotFound, "not_found", "no such browser API")
	})
	return mux
}

// S256 is the PKCE code challenge of verifier (RFC 7636 section 4.2).
func S256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) handleBrowserToken(w http.ResponseWriter, r *http.Request) {
	if !s.tokens.allow(s.clientAddr(r), s.opts.Now()) {
		apiError(w, http.StatusTooManyRequests, codeRateLimited, "too many sign-in attempts; wait a few minutes")
		return
	}
	var body struct {
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
		DeviceID     string `json:"device_id"`
		Instance     string `json:"instance"`
	}
	if !decodeBody(r, &body) || body.GrantType != "authorization_code" || body.Code == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "the body must name grant_type authorization_code and a code")
		return
	}
	bad := func(msg string) { apiError(w, http.StatusBadRequest, codeInvalidGrant, msg) }
	ctx := r.Context()
	g, ok, err := s.store.RedeemBrowserCode(ctx, body.Code)
	if err != nil {
		s.log.Error("redeem browser code", "error", err.Error())
		apiError(w, http.StatusServiceUnavailable, codeInternal, "")
		return
	}
	if !ok {
		bad("the sign-in code is unknown, used or expired; sign in again")
		return
	}
	if !codeVerifierRE.MatchString(body.CodeVerifier) ||
		subtle.ConstantTimeCompare([]byte(S256(body.CodeVerifier)), []byte(g.Challenge)) != 1 {
		s.log.Warn("browser sign-in refused: wrong code verifier", "user", g.User, "device", g.DeviceID)
		bad("the code verifier does not match; sign in again")
		return
	}
	if body.RedirectURI != g.RedirectURI || body.DeviceID != g.DeviceID || body.Instance != g.InstanceID {
		bad("the sign-in code was issued for another redirect, computer or browser; sign in again")
		return
	}
	d, ok, err := s.store.Device(ctx, g.DeviceID)
	if err == nil && ok && d.User == g.User {
		err = s.allowed(ctx, d)
	} else if err == nil {
		err = hub.ErrForbidden
	}
	if err != nil {
		if errors.Is(err, hub.ErrForbidden) || errors.Is(err, hub.ErrUnknownBackend) {
			bad("this computer is no longer connected to Hermes as the account that signed in")
			return
		}
		s.log.Error("check device for browser sign-in", "device", g.DeviceID, "error", err.Error())
		apiError(w, http.StatusServiceUnavailable, codeInternal, "")
		return
	}
	token, sess, err := s.store.CreateBrowserSession(ctx, g.User, g.DeviceID, g.InstanceID, g.Browser)
	if err != nil {
		s.log.Error("create browser sign-in", "error", err.Error())
		apiError(w, http.StatusServiceUnavailable, codeInternal, "")
		return
	}
	s.log.Info("browser signed in", "user", g.User, "device", d.ID, "browser_session", sess.ID, "browser", g.Browser)
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "session_id": sess.ID, "user": g.User,
		keyDeviceID: d.ID, keyDeviceName: d.Name})
}

func (s *Server) handleBrowserRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.revokes.allow(s.clientAddr(r), s.opts.Now()) {
		apiError(w, http.StatusTooManyRequests, codeRateLimited, "too many sign-out requests; wait a few minutes")
		return
	}
	tok := bearer(r)
	if tok == "" {
		apiError(w, http.StatusUnauthorized, codeUnauthorized, "a browser token is required")
		return
	}
	b, ok, err := s.store.RevokeBrowserToken(r.Context(), tok)
	if err != nil {
		s.log.Error("revoke browser sign-in", "error", err.Error())
		apiError(w, http.StatusServiceUnavailable, codeInternal, "")
		return
	}
	if ok {
		s.log.Info("browser signed out", "user", b.User, "device", b.DeviceID, "browser_session", b.ID)
		s.DropBrowser(b.DeviceID, b.ID) //nolint:contextcheck // the leg outlives the request
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
