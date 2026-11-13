package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	clientAddr  = "203.0.113.9"
	proxyPeer   = "10.0.0.2:1234"
	hdrUser     = "Remote-User"
	hdrCFJWT    = "Cf-Access-Jwt-Assertion"
	carolEmail  = "carol@example.test"
	groupFamily = "family"
	nameBob     = "bob"
	nameAlice   = "alice"
	claimEmail  = "email"
)

func TestPasswordHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := auth.NewHasher(auth.Params{Memory: 64, Time: 1, Threads: 1}, 2)
	enc, err := h.Hash(ctx, "correct horse battery")
	if err != nil || !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q %v", enc, err)
	}
	if again, _ := h.Hash(ctx, "correct horse battery"); again == enc {
		t.Fatal("two hashes share a salt")
	}
	if ok, err := h.Verify(ctx, enc, "correct horse battery"); !ok || err != nil {
		t.Fatalf("verify = %v %v", ok, err)
	}
	if ok, _ := h.Verify(ctx, enc, "correct horse batterY"); ok {
		t.Fatal("wrong password accepted")
	}
	if ok, err := h.Verify(ctx, "", "anything at all"); ok || err != nil {
		t.Fatalf("empty hash = %v %v", ok, err)
	}
	for _, bad := range []string{"$2a$10$abc", "$argon2id$v=19$m=99999999,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=18$m=64,t=1,p=1$c2FsdA$a2V5"} {
		if _, err := h.Verify(ctx, bad, "x"); !errors.Is(err, auth.ErrBadHash) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pw, name string
		want     error
	}{
		{"short", nameBob, auth.ErrWeakPassword},
		{"eleven char", nameBob, auth.ErrWeakPassword},
		{"twelve chars", nameBob, nil},
		{"\u00fcn\u00efc\u00f6d\u00e9-pass", nameBob, nil},
		{"Alice.Example", "alice.example", auth.ErrWeakPassword},
		{strings.Repeat("x", 1025), nameBob, auth.ErrLongPassword},
	} {
		if err := auth.CheckPassword(c.pw, c.name); !errors.Is(err, c.want) {
			t.Errorf("%q: %v, want %v", c.pw, err, c.want)
		}
	}
}

func TestLimiter(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	l := auth.NewLimiter(2, time.Minute)
	l.SetClock(func() time.Time { return now })
	for i, want := range []bool{true, true, false} {
		if got := l.Allow("a"); got != want {
			t.Fatalf("try %d = %v", i, got)
		}
	}
	if !l.Allow("b") {
		t.Fatal("keys share a limit")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("a") {
		t.Fatal("window did not slide")
	}
}

func request(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Add(k, v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		remote, xff, want string
	}{
		{"192.0.2.7:1234", clientAddr, "192.0.2.7"},
		{proxyPeer, clientAddr, clientAddr},
		{proxyPeer, "198.51.100.1, 203.0.113.9, 10.0.0.5", clientAddr},
		{proxyPeer, "", "10.0.0.2"},
		{proxyPeer, "junk", "10.0.0.2"},
		{"[::ffff:10.0.0.2]:1234", "2001:db8::1", "2001:db8::1"},
	} {
		got := auth.ClientIP(request(c.remote, map[string]string{"X-Forwarded-For": c.xff}), trusted)
		if got.String() != c.want {
			t.Errorf("%s %q = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestNames(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Alice@Example.com": nameAlice,
		"Bob Smith":         "bob.smith",
		"__x":               "x",
		"日本":                "user",
		"":                  "user",
	} {
		if got := auth.NameFor(in); got != want {
			t.Errorf("NameFor(%q) = %q, want %q", in, got, want)
		}
	}
	if got := auth.NameFor("", "Carol"); got != "carol" {
		t.Errorf("fallback = %q", got)
	}
	if auth.Numbered("a", 1) != "a" || auth.Numbered("a", 3) != "a3" {
		t.Error("numbered")
	}
	if auth.ValidName("Alice") || auth.ValidName(".a") || !auth.ValidName("a.b-c_d") {
		t.Error("valid names")
	}
}

func TestProxyHeaders(t *testing.T) {
	t.Parallel()
	cfg := config.Auth{TrustedProxies: []string{"10.0.0.0/8"}, Proxy: config.Proxy{UserHeader: hdrUser,
		NameHeader: "Remote-Name", EmailHeader: "Remote-Email", GroupsHeader: "Remote-Groups"}}
	p, err := auth.NewProxy(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{hdrUser: nameAlice, "Remote-Name": "Alice", "Remote-Email": "a@example.test", "Remote-Groups": "family, admins|family"}
	id, err := p.Identity(request("10.1.2.3:5", hdr))
	if err != nil || id.ID != nameAlice || id.Display != "Alice" || !id.EmailVerified || !slices.Equal(id.Groups, []string{groupFamily, "admins"}) {
		t.Fatalf("identity = %+v %v", id, err)
	}
	if _, err := p.Identity(request("192.0.2.1:5", hdr)); !errors.Is(err, auth.ErrUntrustedPeer) {
		t.Fatalf("untrusted peer: %v", err)
	}
	if _, err := p.Identity(request("10.1.2.3:5", nil)); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("no header: %v", err)
	}
}

type signer struct {
	srv *httptest.Server
	jose.Signer
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(srv.Close)
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	return &signer{srv: srv, Signer: sig}
}

func (s *signer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := jwt.Signed(s.Signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProxyJWT(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	const iss = "https://team.example.test"
	cfg := config.Auth{Proxy: config.Proxy{UserHeader: hdrUser, JWT: config.JWT{JWKSURL: s.srv.URL, Header: hdrCFJWT,
		Cookie: "CF_Authorization", Issuer: iss, Audiences: []string{"aud-tag"}, UserClaim: claimEmail, NameClaim: "name",
		EmailClaim: claimEmail, GroupsClaim: "groups"}}}
	p, err := auth.NewProxy(cfg, s.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	good := map[string]any{"iss": iss, "aud": []string{"aud-tag"}, "sub": "x1", claimEmail: carolEmail, "name": "Carol",
		"groups": []string{groupFamily}, "exp": now.Add(time.Hour).Unix(), "iat": now.Unix()}
	id, err := p.Identity(request("192.0.2.1:5", map[string]string{hdrCFJWT: s.token(t, good), hdrUser: "mallory"}))
	if err != nil || id.ID != carolEmail || id.Display != "Carol" || !slices.Equal(id.Groups, []string{groupFamily}) {
		t.Fatalf("identity = %+v %v", id, err)
	}
	r := request("192.0.2.1:5", nil)
	r.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: s.token(t, good), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if id, err := p.Identity(r); err != nil || id.ID != carolEmail {
		t.Fatalf("cookie = %+v %v", id, err)
	}
	for name, change := range map[string]func(map[string]any){
		"issuer":   func(c map[string]any) { c["iss"] = "https://evil.example.test" },
		"audience": func(c map[string]any) { c["aud"] = []string{"other"} },
		"expired":  func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() },
		"no user":  func(c map[string]any) { delete(c, claimEmail) },
	} {
		c := map[string]any{}
		for k, v := range good {
			c[k] = v
		}
		change(c)
		if _, err := p.Identity(request("192.0.2.1:5", map[string]string{hdrCFJWT: s.token(t, c)})); !errors.Is(err, auth.ErrBadToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := newSigner(t)
	if _, err := p.Identity(request("192.0.2.1:5", map[string]string{hdrCFJWT: other.token(t, good)})); !errors.Is(err, auth.ErrBadToken) {
		t.Errorf("foreign key: %v", err)
	}
	if _, err := p.Identity(request("192.0.2.1:5", nil)); !errors.Is(err, auth.ErrNoIdentity) {
		t.Errorf("no token: %v", err)
	}
	cfg.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.Proxy.JWT.Header = "Authorization"
	p, _ = auth.NewProxy(cfg, s.srv.Client())
	bearer := map[string]string{"Authorization": "Bearer " + s.token(t, good)}
	if _, err := p.Identity(request("192.0.2.1:5", bearer)); !errors.Is(err, auth.ErrUntrustedPeer) {
		t.Errorf("jwt with trusted proxies from elsewhere: %v", err)
	}
	if id, err := p.Identity(request("10.0.0.1:5", bearer)); err != nil || id.ID != carolEmail {
		t.Errorf("bearer = %+v %v", id, err)
	}
}
