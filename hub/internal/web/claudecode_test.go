package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

type fakeClaudeCode struct {
	mu    sync.Mutex
	names map[string]string
}

func (f *fakeClaudeCode) ClaudeCodeUsage(_ context.Context, user, rng, group string) (hub.ClaudeCodeUsage, error) {
	if rng != "7d" || group != "account" {
		return hub.ClaudeCodeUsage{}, hub.ErrBadRange
	}
	return hub.ClaudeCodeUsage{Range: rng, Group: group, Groups: []hub.ClaudeCodeGroup{{Key: "a", Label: user}}}, nil
}

func (f *fakeClaudeCode) NameClaudeAccount(_ context.Context, user, id, name string) error {
	if id != "acct-1" {
		return hub.ErrBadName
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names[user+"/"+id] = name
	return nil
}

func put(t *testing.T, c *http.Client, u, csrf, origin, body string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", origin)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestClaudeCodeAPI(t *testing.T) {
	t.Parallel()
	fake := &fakeClaudeCode{names: map[string]string{}}
	e := setupWith(t, func(_ *config.Config, opts *web.Options, _ *store.Store) { opts.ClaudeCode = fake })
	c := client(t)
	if resp, _ := get(t, c, e.url+"/api/claude-code/usage?range=7d&group=account"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without login = %d", resp.StatusCode)
	}
	m := login(t, e, c)
	_, body := get(t, c, e.url+"/api/me")
	if !strings.Contains(body, `"claude_code":true`) || !strings.Contains(body, `"usage":true`) {
		t.Fatalf("me = %s", body)
	}
	resp, body := get(t, c, e.url+"/api/claude-code/usage?range=7d&group=account")
	var u hub.ClaudeCodeUsage
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &u) != nil || len(u.Groups) != 1 || u.Groups[0].Label == "" {
		t.Fatalf("usage = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, c, e.url+"/api/claude-code/usage?range=1y&group=account"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad range = %d", resp.StatusCode)
	}
	u0 := e.url + "/api/claude-code/accounts/acct-1/name"
	if got := put(t, c, u0, "", e.url, `{"name":"work"}`); got == http.StatusNoContent {
		t.Fatal("a name was saved without the CSRF token")
	}
	if got := put(t, c, u0, m.CSRF, e.url, `{"name":"work"}`); got != http.StatusNoContent {
		t.Fatalf("name = %d", got)
	}
	if got := put(t, c, e.url+"/api/claude-code/accounts/other/name", m.CSRF, e.url, `{"name":"x"}`); got != http.StatusBadRequest {
		t.Fatalf("bad account = %d", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.names) != 1 || fake.names[u.Groups[0].Label+"/acct-1"] != "work" {
		t.Fatalf("names %v", fake.names)
	}
}
