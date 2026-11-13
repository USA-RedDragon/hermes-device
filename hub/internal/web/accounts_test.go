package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

const (
	alicePhrase   = "alice's long password"
	bobPhrase     = "bob's long password!"
	resetPhrase   = "reset by the admin"
	headerUser    = "Remote-User"
	aliceIdentity = "alice@example.test"
	userCarol     = "carol"
	fieldBackends = "backends"
	fieldPassword = "password"
)

// testHasher is a cheap hasher for tests.
func testHasher() *auth.Hasher {
	return auth.NewHasher(auth.Params{Memory: 64, Time: 1, Threads: 1}, 4)
}

// setupLocal is a hub in auth.mode local with no accounts yet.
func setupLocal(t *testing.T) *hubEnv {
	t.Helper()
	return setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.Auth = config.Auth{Mode: config.AuthLocal, Local: config.Local{MaxFailures: 3}}
		cfg.OIDC, cfg.Users, cfg.UI.DefaultBackends = config.OIDC{}, nil, nil
		cfg.Backends[0].Users = nil
		opts.Accounts, opts.Hasher = st, testHasher()
	})
}

type meAuth struct {
	User     string `json:"user"`
	CSRF     string `json:"csrf"`
	Auth     string `json:"auth"`
	Admin    bool   `json:"admin"`
	Password bool   `json:"password"`
}

func meOf(t *testing.T, e *hubEnv, c *http.Client) (meAuth, int) {
	t.Helper()
	resp, body := get(t, c, e.url+"/api/me")
	var m meAuth
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
	}
	return m, resp.StatusCode
}

func passwordLogin(t *testing.T, e *hubEnv, c *http.Client, name, password string) int {
	t.Helper()
	resp, _ := post(t, c, e.url+"/auth/password", "", e.url, map[string]string{fieldName: name, fieldPassword: password, "next": "/x"})
	return resp.StatusCode
}

func TestLocalSetupAndLogin(t *testing.T) {
	t.Parallel()
	e := setupLocal(t)
	c := client(t)
	if resp, body := get(t, c, e.url+"/auth/info"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"setup":true`) ||
		!strings.Contains(body, `"mode":"local"`) || strings.Contains(body, "token") {
		t.Fatalf("info = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, c, e.url+"/auth/login?next=/a"); resp.Request.URL.Path != "/setup" {
		t.Fatalf("login with no account = %s, want /setup", resp.Request.URL)
	}
	if resp, _ := get(t, c, e.url+"/"); resp.Request.URL.Path != "/setup" {
		t.Fatalf("root with no account = %s, want /setup", resp.Request.URL)
	}
	first := map[string]string{fieldName: "Alice", "display": "Alice A", fieldPassword: alicePhrase}
	if resp, _ := post(t, c, e.url+"/auth/setup", "", "https://evil.example", first); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin setup = %d", resp.StatusCode)
	}
	first["password"] = "short"
	if resp, body := post(t, c, e.url+"/auth/setup", "", e.url, first); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at least 12") {
		t.Fatalf("weak password = %d %s", resp.StatusCode, body)
	}
	first["password"] = alicePhrase
	if resp, body := post(t, c, e.url+"/auth/setup", "", e.url, first); resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup = %d %s", resp.StatusCode, body)
	}
	if m, code := meOf(t, e, c); code != http.StatusOK || m.User != userAlice || !m.Admin || !m.Password || m.Auth != config.AuthLocal {
		t.Fatalf("me after setup = %d %+v", code, m)
	}
	if _, body := get(t, c, e.url+"/api/backends"); !strings.Contains(body, `"pc"`) || !strings.Contains(body, `"home"`) {
		t.Fatalf("the first admin gets every backend: %s", body)
	}
	if resp, _ := post(t, client(t), e.url+"/auth/setup", "", e.url, first); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second setup = %d", resp.StatusCode)
	}
	if _, body := get(t, c, e.url+"/auth/info"); !strings.Contains(body, `"setup":false`) {
		t.Fatalf("info after setup = %s", body)
	}

	if resp, _ := get(t, client(t), e.url+"/auth/login?next=/a"); resp.Request.URL.Path != "/login" || resp.Request.URL.Query().Get("next") != "/a" {
		t.Fatalf("login redirect = %s", resp.Request.URL)
	}
	if resp, _ := get(t, c, e.url+"/"); resp.Request.URL.Path != "/" {
		t.Fatalf("root after setup = %s", resp.Request.URL)
	}

	c2 := client(t)
	if code := passwordLogin(t, e, c2, userAlice, "not the password"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", code)
	}
	if code := passwordLogin(t, e, c2, "nobody", alicePhrase); code != http.StatusUnauthorized {
		t.Fatalf("unknown user = %d", code)
	}
	if resp, body := post(t, c2, e.url+"/auth/password", "", "https://evil.example", map[string]string{fieldName: userAlice, fieldPassword: alicePhrase}); resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(body, "Open Talaria at "+e.url+", or set TALARIA_PUBLIC_URL to this address.") {
		t.Fatalf("cross-origin login = %d %s", resp.StatusCode, body)
	}
	if code := passwordLogin(t, e, c2, " ALICE ", alicePhrase); code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	if m, code := meOf(t, e, c2); code != http.StatusOK || m.User != userAlice {
		t.Fatalf("me = %d %+v", code, m)
	}
}

func TestLocalLockout(t *testing.T) {
	t.Parallel()
	e := setupLocal(t)
	ctx := context.Background()
	hash, _ := testHasher().Hash(ctx, alicePhrase)
	if err := e.store.CreateAccount(ctx, store.Account{Name: userAlice, PasswordHash: hash, Admin: true}); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	for range 3 {
		if code := passwordLogin(t, e, c, userAlice, "wrong password!"); code != http.StatusUnauthorized {
			t.Fatalf("wrong = %d", code)
		}
	}
	if code := passwordLogin(t, e, c, userAlice, alicePhrase); code != http.StatusUnauthorized {
		t.Fatalf("locked account logged in: %d", code)
	}
	for range 6 {
		passwordLogin(t, e, c, "nobody", "x")
	}
	if code := passwordLogin(t, e, c, "nobody", "x"); code != http.StatusTooManyRequests {
		t.Fatalf("eleventh try from one address = %d", code)
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	t.Parallel()
	e := setupLocal(t)
	ctx := context.Background()
	hash, _ := testHasher().Hash(ctx, alicePhrase)
	if err := e.store.CreateAccount(ctx, store.Account{Name: userAlice, PasswordHash: hash, Admin: true}); err != nil {
		t.Fatal(err)
	}
	a, other := client(t), client(t)
	for _, c := range []*http.Client{a, other} {
		if code := passwordLogin(t, e, c, userAlice, alicePhrase); code != http.StatusOK {
			t.Fatalf("login = %d", code)
		}
	}
	m, _ := meOf(t, e, a)
	if resp, _ := post(t, a, e.url+"/api/account/password", m.CSRF, "", map[string]string{"current": "wrong", fieldPassword: "a brand new password"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong current = %d", resp.StatusCode)
	}
	if resp, body := post(t, a, e.url+"/api/account/password", m.CSRF, "", map[string]string{"current": alicePhrase, fieldPassword: "a brand new password"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change = %d %s", resp.StatusCode, body)
	}
	if _, code := meOf(t, e, a); code != http.StatusOK {
		t.Fatalf("this browser after change = %d", code)
	}
	if _, code := meOf(t, e, other); code != http.StatusUnauthorized {
		t.Fatalf("other browser after change = %d", code)
	}
	if code := passwordLogin(t, e, client(t), userAlice, "a brand new password"); code != http.StatusOK {
		t.Fatalf("login with new password = %d", code)
	}
}

func TestAdminManagesUsers(t *testing.T) {
	t.Parallel()
	e := setupLocal(t)
	ctx := context.Background()
	hash, _ := testHasher().Hash(ctx, alicePhrase)
	if err := e.store.CreateAccount(ctx, store.Account{Name: userAlice, PasswordHash: hash, Admin: true, Backends: []string{"pc"}}); err != nil {
		t.Fatal(err)
	}
	admin := client(t)
	passwordLogin(t, e, admin, userAlice, alicePhrase)
	m, _ := meOf(t, e, admin)
	add := func(body map[string]any) (int, string) {
		resp, b := post(t, admin, e.url+"/api/admin/users", m.CSRF, "", body)
		return resp.StatusCode, b
	}
	if code, _ := add(map[string]any{fieldName: userBob, fieldPassword: bobPhrase, fieldBackends: []string{"nowhere"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown backend = %d", code)
	}
	if code, body := add(map[string]any{fieldName: userBob, "display": "Bob", fieldPassword: bobPhrase, fieldBackends: []string{backendHome}}); code != http.StatusCreated {
		t.Fatalf("add bob = %d %s", code, body)
	}
	if code, _ := add(map[string]any{fieldName: userBob, fieldPassword: bobPhrase}); code != http.StatusConflict {
		t.Fatalf("add bob twice = %d", code)
	}
	_, body := get(t, admin, e.url+"/api/admin/users")
	var list struct {
		Users []struct {
			Name     string   `json:"name"`
			Backends []string `json:"backends"`
			Admin    bool     `json:"admin"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Users) != 2 || list.Users[1].Name != userBob || list.Users[1].Backends[0] != backendHome {
		t.Fatalf("users = %s", body)
	}

	bob := client(t)
	if code := passwordLogin(t, e, bob, userBob, bobPhrase); code != http.StatusOK {
		t.Fatalf("bob login = %d", code)
	}
	if _, body := get(t, bob, e.url+"/api/backends"); body != `[{"id":"home","name":"home"}]`+"\n" {
		t.Fatalf("bob's backends = %s", body)
	}
	if resp, _ := get(t, bob, e.url+"/api/admin/users"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob lists users = %d", resp.StatusCode)
	}
	edit := func(name string, body map[string]any) int {
		resp, _ := post(t, admin, e.url+"/api/admin/users/"+name, m.CSRF, "", body)
		return resp.StatusCode
	}
	if code := edit(userAlice, map[string]any{"admin": false}); code != http.StatusBadRequest {
		t.Fatalf("demote self = %d", code)
	}
	if code := edit(userBob, map[string]any{fieldBackends: []string{"pc", backendHome}}); code != http.StatusNoContent {
		t.Fatalf("grant = %d", code)
	}
	if _, body := get(t, bob, e.url+"/api/backends"); !strings.Contains(body, `"pc"`) {
		t.Fatalf("bob after grant = %s", body)
	}
	if code := edit(userBob, map[string]any{"disabled": true}); code != http.StatusNoContent {
		t.Fatalf("disable = %d", code)
	}
	if _, code := meOf(t, e, bob); code != http.StatusUnauthorized {
		t.Fatalf("disabled bob's session = %d", code)
	}
	if code := passwordLogin(t, e, client(t), userBob, bobPhrase); code != http.StatusUnauthorized {
		t.Fatalf("disabled bob logs in = %d", code)
	}
	if code := edit(userBob, map[string]any{"disabled": false}); code != http.StatusNoContent {
		t.Fatalf("enable = %d", code)
	}
	if resp, _ := post(t, admin, e.url+"/api/admin/users/bob/password", m.CSRF, "", map[string]string{fieldPassword: resetPhrase}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", resp.StatusCode)
	}
	if code := passwordLogin(t, e, client(t), userBob, resetPhrase); code != http.StatusOK {
		t.Fatalf("login after reset = %d", code)
	}
	if resp, _ := post(t, admin, e.url+"/api/admin/users/alice/delete", m.CSRF, "", map[string]string{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete self = %d", resp.StatusCode)
	}
	if resp, _ := post(t, admin, e.url+"/api/admin/users/bob/delete", m.CSRF, "", map[string]string{}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete bob = %d", resp.StatusCode)
	}
	if _, code := meOf(t, e, bob); code != http.StatusUnauthorized {
		t.Fatalf("deleted bob's session = %d", code)
	}
}

// proxyClient sends the proxy's headers with every request.
type proxyClient struct {
	base    http.RoundTripper
	headers map[string]string
}

func (p *proxyClient) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range p.headers {
		r.Header.Set(k, v)
	}
	return p.base.RoundTrip(r)
}

func behindProxy(t *testing.T, headers map[string]string) (*http.Client, *proxyClient) {
	t.Helper()
	c := client(t)
	p := &proxyClient{base: http.DefaultTransport, headers: headers}
	c.Transport = p
	return c, p
}

func setupProxy(t *testing.T, fn func(cfg *config.Config)) *hubEnv {
	t.Helper()
	return setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.Auth = config.Auth{Mode: config.AuthProxy, TrustedProxies: []string{"127.0.0.0/8", "::1"},
			Proxy: config.Proxy{UserHeader: headerUser, NameHeader: "Remote-Name", EmailHeader: "Remote-Email", GroupsHeader: "Remote-Groups"}}
		cfg.OIDC = config.OIDC{}
		cfg.Users = []config.User{{Sub: aliceIdentity, Name: userAlice}, {Sub: "bob@example.test", Name: userBob}}
		opts.Accounts, opts.Hasher = st, testHasher()
		if fn != nil {
			fn(cfg)
		}
	})
}

func TestProxyLogin(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, nil)
	c, p := behindProxy(t, map[string]string{headerUser: aliceIdentity, "Remote-Name": "Alice"})
	if resp, _ := get(t, c, e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
		t.Fatalf("proxy login ended at %s with %d", resp.Request.URL, resp.StatusCode)
	}
	if m, code := meOf(t, e, c); code != http.StatusOK || m.User != userAlice || m.Auth != config.AuthProxy || m.Password {
		t.Fatalf("me = %d %+v", code, m)
	}
	p.headers[headerUser] = "bob@example.test"
	if _, code := meOf(t, e, c); code != http.StatusUnauthorized {
		t.Fatalf("alice's cookie with bob's header = %d", code)
	}
	delete(p.headers, headerUser)
	if _, code := meOf(t, e, c); code != http.StatusUnauthorized {
		t.Fatalf("cookie without the header = %d", code)
	}
	carol, _ := behindProxy(t, map[string]string{headerUser: userCarol})
	if resp, _ := get(t, carol, e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown identity = %d", resp.StatusCode)
	}
	if resp, _ := post(t, carol, e.url+"/auth/password", "", e.url, map[string]string{fieldName: userCarol, fieldPassword: "x"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("password login in proxy mode = %d", resp.StatusCode)
	}
}

func TestProxyUntrustedPeer(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, func(cfg *config.Config) { cfg.Auth.TrustedProxies = []string{"10.0.0.0/8"} })
	c, _ := behindProxy(t, map[string]string{headerUser: aliceIdentity})
	if resp, _ := get(t, c, e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("header from an untrusted peer = %d", resp.StatusCode)
	}
}

func TestProxyAutoCreate(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, func(cfg *config.Config) {
		cfg.Auth.Proxy.AutoCreate = true
		cfg.Users = nil
		cfg.UI.DefaultBackends = nil
		cfg.Backends[0].Users = nil
	})
	carol, _ := behindProxy(t, map[string]string{headerUser: "Carol@Example.test", "Remote-Groups": groupFamily})
	if resp, _ := get(t, carol, e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("auto create = %d", resp.StatusCode)
	}
	m, _ := meOf(t, e, carol)
	if m.User != userCarol || !m.Admin {
		t.Fatalf("first auto account = %+v", m)
	}
	if _, body := get(t, carol, e.url+"/api/backends"); body != `[{"id":"home","name":"home"}]`+"\n" {
		t.Fatalf("carol's backends by group = %s", body)
	}
	dave, _ := behindProxy(t, map[string]string{headerUser: "carol@other.test"})
	get(t, dave, e.url+"/auth/login?next=/")
	if m, _ := meOf(t, e, dave); m.User != "carol2" || m.Admin {
		t.Fatalf("second auto account = %+v", m)
	}
}

func TestOIDCAutoCreateAndPreparedAccounts(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.OIDC.AutoCreate, cfg.OIDC.AllowedDomains, cfg.OIDC.EmailClaim = true, []string{"example.test"}, "email"
		cfg.Auth.Admins = []string{userAlice}
		opts.Accounts, opts.Hasher = st, testHasher()
	})
	ctx := context.Background()
	if err := e.store.CreateAccount(ctx, store.Account{Name: "erin", Email: "erin@elsewhere.test", Backends: []string{"pc"}}); err != nil {
		t.Fatal(err)
	}
	asWith := func(sub, user, email string, verified bool) {
		e.is.as(sub, user)
		e.is.mu.Lock()
		e.is.email, e.is.verified = email, verified
		e.is.mu.Unlock()
	}

	alice := client(t)
	if m := login(t, e, alice); m.User != userAlice {
		t.Fatalf("users[] login = %+v", m)
	}
	if m, _ := meOf(t, e, alice); !m.Admin {
		t.Fatal("auth.admins names alice")
	}

	asWith("sub-erin", "Erin", "erin@elsewhere.test", true)
	if m := login(t, e, client(t)); m.User != "erin" {
		t.Fatalf("prepared account by email = %+v", m)
	}
	if a, _, _ := e.store.Account(ctx, "erin"); a.Identity != "oidc:sub-erin" {
		t.Fatalf("erin not bound: %+v", a)
	}

	asWith("sub-frank", "frank", "frank@example.test", false)
	if resp, _ := get(t, client(t), e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unverified email = %d", resp.StatusCode)
	}
	asWith("sub-frank", "frank", "frank@example.test", true)
	if m := login(t, e, client(t)); m.User != "frank" {
		t.Fatalf("auto account = %+v", m)
	}
	if a, _, _ := e.store.Account(ctx, "frank"); a.Admin {
		t.Fatal("auto account is admin although auth.admins names someone")
	}
	asWith("sub-gina", "gina", "gina@other.test", true)
	if resp, _ := get(t, client(t), e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other domain = %d", resp.StatusCode)
	}
}

// A config from before auth.admins names no admin; its first user is one, so
// Settings > Users works after the upgrade with no new setting.
func TestOIDCFirstConfigUserIsAdminWithoutAuthAdmins(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(_ *config.Config, opts *web.Options, st *store.Store) {
		opts.Accounts, opts.Hasher = st, testHasher()
	})
	alice := client(t)
	login(t, e, alice)
	if m, _ := meOf(t, e, alice); !m.Admin {
		t.Fatal("the first users[] entry is not an admin when auth.admins is empty")
	}
}
