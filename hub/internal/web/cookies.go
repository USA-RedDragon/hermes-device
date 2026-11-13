package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

var errBadCookie = errors.New("bad cookie")

// sealer encrypts and authenticates cookie values with AES-GCM. Each
// purpose gets its own key, so a login cookie never opens as a session.
type sealer struct {
	aead cipher.AEAD
	name string
}

func newSealer(secret, purpose string) (*sealer, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("hermes-hub cookie " + purpose))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead, name: purpose}, nil
}

func (s *sealer) seal(v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, []byte(s.name))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (s *sealer) open(value string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return errBadCookie
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(s.name))
	if err != nil {
		return errBadCookie
	}
	return json.Unmarshal(plain, v)
}

// session is the content of the session cookie.
type session struct {
	User    string   `json:"u"`
	Display string   `json:"d"`
	Groups  []string `json:"g"`
	CSRF    string   `json:"c"`
	Expires int64    `json:"e"`
	// Account is set for a user with an account in the hub's database,
	// whose sessions end when it is disabled or its Epoch goes up.
	Account bool  `json:"a,omitempty"`
	Epoch   int64 `json:"p,omitempty"`
	// Identity is the proxy or OIDC identity of the login. In auth.mode
	// proxy every request must come with the same one.
	Identity string `json:"i,omitempty"`
}

// login is the content of the short-lived cookie that carries the OIDC
// state, nonce and PKCE verifier between /auth/login and /auth/callback.
type login struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

const loginTTL = 10 * time.Minute

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) cookieName(bare string) string {
	if s.secure {
		return "__Host-" + bare
	}
	return bare
}

func (s *Server) setCookie(w http.ResponseWriter, bare, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows http.public_url; only a plain-http public URL turns it off
		Name: s.cookieName(bare), Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, bare string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // as in setCookie
		Name: s.cookieName(bare), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) session(r *http.Request) (*session, bool) {
	c, err := r.Cookie(s.cookieName(sessionCookie))
	if err != nil {
		return nil, false
	}
	var sess session
	if s.sessions.open(c.Value, &sess) != nil || sess.User == "" || time.Now().Unix() >= sess.Expires {
		return nil, false
	}
	if sess.Account && !s.accountOK(r.Context(), sess.User, sess.Epoch) {
		return nil, false
	}
	if s.proxy != nil {
		id, err := s.proxy.Identity(r)
		if err != nil || id.ID != sess.Identity {
			return nil, false
		}
	}
	return &sess, true
}
