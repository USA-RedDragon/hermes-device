package web_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// issuer is a fake OIDC provider: discovery, authorize (logs the user in
// at once), token with PKCE check, JWKS.
type issuer struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	secret   string

	mu       sync.Mutex
	user     string
	sub      string
	groups   []string
	codes    map[string]grant
	badNonce bool
}

type grant struct {
	nonce, challenge, redirect string
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key, clientID: clientID, secret: "s3cret", user: userAlice, groups: []string{groupAdmins}, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		u := is.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
			"jwks_uri": u + "/jwks", "userinfo_endpoint": u + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &is.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != is.clientID || q.Get("code_challenge_method") != methodS256 || q.Get("code_challenge") == "" ||
			q.Get("response_type") != responseTypeCode {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		code := randomString()
		is.mu.Lock()
		is.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		is.mu.Unlock()
		target := q.Get("redirect_uri") + "?code=" + code + "&state=" + url.QueryEscape(q.Get("state"))
		http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // fake issuer in tests
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		is.mu.Lock()
		g, found := is.codes[r.PostForm.Get(fieldCode)]
		delete(is.codes, r.PostForm.Get(fieldCode))
		user, groups, badNonce, sub := is.user, is.groups, is.badNonce, is.sub
		is.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if id != is.clientID || sec != is.secret || !found || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge ||
			r.PostForm.Get("redirect_uri") != g.redirect {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if sub == "" {
			sub = "sub-" + user
		}
		nonce := g.nonce
		if badNonce {
			nonce = "other"
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: is.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		tok, _ := jwt.Signed(signer).Claims(jwt.Claims{
			Issuer: is.srv.URL, Subject: sub, Audience: jwt.Audience{is.clientID},
			Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now()),
		}).Claims(map[string]any{"nonce": nonce, "preferred_username": user, "groups": groups}).Serialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": tok})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
