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
