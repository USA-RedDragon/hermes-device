package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
)

const (
	pageLogin    = "/login"
	pageSetup    = "/setup"
	pageTelegram = "/link/telegram"
	settingsHint = "Settings &gt; Users"
	patIdentity  = "pat@example.test"
	headerGroups = "Remote-Groups"
	hostileID    = "<img src=x onerror=alert(1)>"
)

// /login and /setup are static pages whose script asks /auth/info. A hub
// with no account sends /login to /setup; a person who is logged in does
// not need the login page.
func TestLoginAndSetupPages(t *testing.T) {
	t.Parallel()
	e := setupLocal(t)
	c := client(t)
	resp, body := get(t, c, e.url+"/login?next=/a")
	if resp.Request.URL.Path != pageSetup || resp.StatusCode != http.StatusOK {
		t.Fatalf("login with no account ended at %s with %d", resp.Request.URL, resp.StatusCode)
	}
	for _, want := range []string{`data-page="setup"`, "/static/auth.js", "/static/authrules.js"} {
		if !strings.Contains(body, want) {
			t.Fatalf("setup page lacks %q: %s", want, body)
		}
	}
	if resp.Header.Get("Cache-Control") != noStore {
		t.Fatalf("setup page cache = %q", resp.Header.Get("Cache-Control"))
	}
	if resp, body := get(t, c, e.url+"/static/auth.js"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "HubAuth") {
		t.Fatalf("auth.js = %d", resp.StatusCode)
	}

	first := map[string]string{fieldName: userAlice, fieldPassword: alicePhrase}
	if resp, body := post(t, c, e.url+"/auth/setup", "", e.url, first); resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup = %d %s", resp.StatusCode, body)
	}
	// An account exists: the setup page still loads, its script says the hub is set up, and the login page shows.
	if resp, body := get(t, client(t), e.url+"/setup"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `data-page="setup"`) {
		t.Fatalf("setup page after setup = %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, client(t), e.url+"/login?next=/a")
	if resp.Request.URL.Path != pageLogin || !strings.Contains(body, `data-page="login"`) {
		t.Fatalf("login page ended at %s: %s", resp.Request.URL, body)
	}
	// A logged-in browser goes on to next, and only to a path on this site.
	if resp, _ := get(t, c, e.url+pageLogin+"?next="+pageTelegram); resp.Request.URL.Path != pageTelegram {
		t.Fatalf("logged in /login ended at %s", resp.Request.URL)
	}
	if resp, _ := get(t, c, e.url+"/login?next=//evil.example/x"); resp.Request.URL.Host != strings.TrimPrefix(e.url, "http://") || resp.Request.URL.Path != "/" {
		t.Fatalf("open redirect: ended at %s", resp.Request.URL)
	}
}

// In proxy and OIDC mode the hub has no login page and no first user.
func TestOtherModesHaveNoLoginPage(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, nil)
	c, _ := behindProxy(t, map[string]string{headerUser: aliceIdentity})
	if resp, _ := get(t, c, e.url+pageLogin+"?next="+pageTelegram); resp.Request.URL.Path != pageTelegram {
		t.Fatalf("proxy /login ended at %s, want the login through the proxy and then next", resp.Request.URL)
	}
	if resp, body := get(t, c, e.url+"/setup"); resp.Request.URL.Path != "/" || strings.Contains(body, `data-page="setup"`) {
		t.Fatalf("proxy /setup ended at %s", resp.Request.URL)
	}
	o := setup(t)
	if resp, _ := get(t, client(t), o.url+"/login"); resp.Request.URL.Host == strings.TrimPrefix(o.url, "http://") && resp.Request.URL.Path == pageLogin {
		t.Fatalf("oidc /login stayed on the hub's login page: %s", resp.Request.URL)
	}
}

// The refused page names the identity so an admin can add it, escapes it
// and offers the proxy's logout page when there is one.
func TestRefusedPageNamesIdentity(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, func(cfg *config.Config) { cfg.Auth.Proxy.LogoutURL = "https://proxy.example.test/logout" })
	c, _ := behindProxy(t, map[string]string{headerUser: patIdentity})
	resp, body := get(t, c, e.url+"/auth/login?next=/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{"<h1>Not set up</h1>", "<code>" + patIdentity + "</code>", "this hub has no account for you", settingsHint,
		`href="https://proxy.example.test/logout"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("refused page lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Ask the owner") {
		t.Fatalf("the old text is still there: %s", body)
	}

	hostile, _ := behindProxy(t, map[string]string{headerUser: hostileID})
	if _, body := get(t, hostile, e.url+"/auth/login?next=/"); strings.Contains(body, "<img") || !strings.Contains(body, "&lt;img") {
		t.Fatalf("the identity is not escaped: %s", body)
	}

	// No proxy logout page: no Log out link.
	plain := setupProxy(t, nil)
	c2, _ := behindProxy(t, map[string]string{headerUser: patIdentity, "Remote-Email": patIdentity})
	if _, body := get(t, c2, plain.url+"/auth/login?next=/"); strings.Contains(body, "Log out") {
		t.Fatalf("Log out with nowhere to go: %s", body)
	}
}

// A proxy request without a trusted identity gets a page with what to
// check, as a list, a link to the setup help and Try again.
func TestProxyErrorPage(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, func(cfg *config.Config) { cfg.Auth.TrustedProxies = []string{"10.0.0.0/8"} })
	c, _ := behindProxy(t, map[string]string{headerUser: aliceIdentity})
	resp, body := get(t, c, e.url+"/auth/login?next=/a")
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"<h1>The proxy did not say who you are</h1>", "<ol class=\"checks\">", "<code>auth.trusted_proxies</code>",
		"<code>auth.proxy.user_header</code>", "<code>auth.proxy.jwt</code>", "Show setup help",
		"https://usa-reddragon.github.io/talaria/hub/auth.html#behind-a-proxy-authmode-proxy", `href="/auth/login?next=%2Fa"`, "Try again"} {
		if !strings.Contains(body, want) {
			t.Fatalf("proxy error page lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "<pre") {
		t.Fatalf("the page has a pre block: %s", body)
	}
	// The address the hub saw is in its log, not on the page.
	if strings.Contains(body, "127.0.0.1") {
		t.Fatalf("the page shows the peer address: %s", body)
	}
}

// Settings learns how people log in from /api/me and the users list.
func TestLoginInfoForSettings(t *testing.T) {
	t.Parallel()
	e := setupProxy(t, func(cfg *config.Config) {
		cfg.Auth.Admins = []string{userAlice}
		cfg.Auth.Proxy.AutoCreate = true
	})
	c, _ := behindProxy(t, map[string]string{headerUser: aliceIdentity, headerGroups: groupFamily})
	if resp, _ := get(t, c, e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"identity":"`+aliceIdentity+`"`) {
		t.Fatalf("me lacks the identity: %s", body)
	}
	_, body := get(t, c, e.url+"/api/admin/users")
	for _, want := range []string{`"login":{`, `"header":"Remote-User"`, `"trusted":["127.0.0.0/8","::1/128"]`, `"auto_create":true`, `"groups":["` + groupFamily + `"]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("users list lacks %q: %s", want, body)
		}
	}
}

// An OIDC hub says which provider logs people in.
func TestOIDCLoginInfo(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	if resp, _ := get(t, c, e.url+"/auth/login?next=/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	host := strings.TrimPrefix(e.is.srv.URL, "http://")
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"issuer":"`+host+`"`) || !strings.Contains(body, `"auth":"oidc"`) {
		t.Fatalf("me lacks the issuer %q: %s", host, body)
	}
}
