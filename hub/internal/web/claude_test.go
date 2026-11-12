package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/claude"
	"github.com/USA-RedDragon/hermes-hub/internal/claude/claudetest"
	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

func do(t *testing.T, c *http.Client, method, u, csrf, origin string) (*http.Response, string) {
	t.Helper()
	if method == http.MethodPost {
		return post(t, c, u, csrf, origin, map[string]string{})
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, u, nil)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const (
	signIn1     = "si1"
	fieldSignIn = "signin_id"
)

func TestClaudeAPI(t *testing.T) {
	t.Parallel()
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		cfg.Backends[0].Users = []string{userAlice, userBob}
		cfg.Claude.Admins = []string{"alice=pc"}
		admins, _ := cfg.Claude.AdminMap()
		d := gateway.NewDialer(gateway.Credentials{BaseURL: cfg.Backends[0].URL, Provider: "basic", Username: clientID, Password: "pw"})
		opts.Claude = claude.New(claude.Options{Access: lateAccess{&e}, Admins: admins, Requesters: map[string]device.Requester{"pc": d}})
	})
	plugin := claudetest.New(e.gw)
	base := e.url + "/api/claude/pc/accounts"

	alice := client(t)
	m := login(t, e, alice)
	if _, body := get(t, alice, e.url+"/api/me"); !strings.Contains(body, `"claude":true`) || !strings.Contains(body, `"claude_admin":true`) {
		t.Fatalf("me = %s", body)
	}
	if resp, body := get(t, alice, base+"?usage=1"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"admin":true`) ||
		!strings.Contains(body, `"provider":"claude-account1"`) {
		t.Fatalf("accounts = %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, alice, http.MethodPost, base+"/account1/signin", "", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("start without CSRF = %d", resp.StatusCode)
	}
	if resp, _ := do(t, alice, http.MethodPost, base+"/account1/signin", m.CSRF, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("start cross origin = %d", resp.StatusCode)
	}
	if resp, _ := do(t, alice, http.MethodDelete, base+"/account1/signin", "", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cancel without CSRF = %d", resp.StatusCode)
	}
	if plugin.CallCount("POST") != 0 || plugin.CallCount("DELETE") != 0 {
		t.Fatal("a refused request reached the plugin")
	}
	resp, body := do(t, alice, http.MethodPost, base+"/account1/signin", m.CSRF, e.url)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"url":"https://claude.com/cai/oauth/authorize`) {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	code := base + "/account1/signin/code"
	if resp, _ := post(t, alice, code, "", "", map[string]string{fieldSignIn: signIn1, fieldCode: claudetest.GoodCode}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("code without CSRF = %d", resp.StatusCode)
	}
	resp, body = post(t, alice, code, m.CSRF, e.url, map[string]string{fieldSignIn: signIn1, fieldCode: "bad"})
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(body, "bad\"") {
		t.Fatalf("bad code = %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, alice, code, m.CSRF, e.url, map[string]string{fieldSignIn: "old", fieldCode: claudetest.GoodCode})
	if resp.StatusCode != http.StatusConflict || strings.Contains(body, claudetest.GoodCode) {
		t.Fatalf("stale sign-in = %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, alice, code, m.CSRF, e.url, map[string]string{fieldSignIn: signIn1, fieldCode: claudetest.GoodCode})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"state":"succeeded"`) || !strings.Contains(body, "alice@example.com") ||
		strings.Contains(body, claudetest.GoodCode) || resp.Header.Get("Cache-Control") != noStore {
		t.Fatalf("code = %d %s", resp.StatusCode, body)
	}

	bob := client(t)
	e.is.as("sub-bob", userBob, "family")
	mb := login(t, e, bob)
	if _, body := get(t, bob, base); !strings.Contains(body, `"admin":false`) {
		t.Fatalf("bob's accounts = %s", body)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		if resp, _ := do(t, bob, method, base+"/account1/signin", mb.CSRF, e.url); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bob %s sign-in = %d", method, resp.StatusCode)
		}
	}
	if resp, _ := post(t, bob, code, mb.CSRF, e.url, map[string]string{fieldSignIn: signIn1, fieldCode: claudetest.GoodCode}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob code = %d", resp.StatusCode)
	}
	if resp, _ := get(t, bob, e.url+"/api/claude/home/accounts"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("backend without the plugin = %d", resp.StatusCode)
	}
}

func TestClaudePoolAPI(t *testing.T) {
	t.Parallel()
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		cfg.Backends[0].Users = []string{userAlice}
		d := gateway.NewDialer(gateway.Credentials{BaseURL: cfg.Backends[0].URL, Provider: providerBasic, Username: clientID, Password: "pw"})
		opts.Claude = claude.New(claude.Options{Access: lateAccess{&e}, Requesters: map[string]device.Requester{"pc": d}})
	})
	plugin := claudetest.New(e.gw)
	plugin.Lock()
	plugin.Pools["s1"] = claude.Pool{Account: "account2", Pinned: true}
	plugin.Unlock()
	alice := client(t)
	login(t, e, alice)
	if resp, body := get(t, alice, e.url+"/api/claude/pc/pool?session=s1&model=m"); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"account":"account2"`) || !strings.Contains(body, `"pinned":true`) {
		t.Fatalf("pool = %d %s", resp.StatusCode, body)
	}
	if n := plugin.CallCount("GET /pool?model=m&session=s1"); n != 1 {
		t.Fatalf("plugin calls = %v", plugin.Calls)
	}
}

func TestNewChatModelsAPI(t *testing.T) {
	t.Parallel()
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		cfg.Backends[0].Users = []string{userAlice}
		d := gateway.NewDialer(gateway.Credentials{BaseURL: cfg.Backends[0].URL, Provider: providerBasic, Username: clientID, Password: "pw"})
		opts.Claude = claude.New(claude.Options{Access: lateAccess{&e}, Requesters: map[string]device.Requester{"pc": d}})
	})
	opts := claudetest.NewChatGPT(e.gw)
	yes := true
	opts.Lock()
	opts.Model, opts.Provider, opts.ContextLength = "claude-sonnet-5-5[1m]", "claude-pool", 1000000
	opts.Rows = []claude.ProviderRow{{Slug: "claude-pool", Authenticated: &yes, Models: []string{"claude-sonnet-5-5[1m]"}}}
	opts.Unlock()
	alice := client(t)
	login(t, e, alice)
	resp, body := get(t, alice, e.url+"/api/backends/pc/models")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"model":"claude-sonnet-5-5[1m]"`) ||
		!strings.Contains(body, `"provider":"claude-pool"`) || !strings.Contains(body, `"context_length":1000000`) ||
		!strings.Contains(body, `"efforts":[`) {
		t.Fatalf("models = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, alice, e.url+"/api/backends/home/models"); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("models of a backend alice cannot use = %d", resp.StatusCode)
	}
	if resp, _ := get(t, client(t), e.url+"/api/backends/pc/models"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous models = %d", resp.StatusCode)
	}
}
