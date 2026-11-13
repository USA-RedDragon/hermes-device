package web_test

import (
	"bytes"
	"context"
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

type fakeSubagents struct {
	mu     sync.Mutex
	choice map[string]string
}

func (f *fakeSubagents) answer(session string) hub.Subagents {
	c := f.choice[session]
	if c == "" {
		c = hub.SubagentsAuto
	}
	return hub.Subagents{Choice: c, Choices: []hub.SubagentChoice{{ID: hub.SubagentsAuto, Label: "Auto (model chooses)"},
		{ID: "haiku", Label: "Haiku 5.5", Group: "Every subagent on"}}}
}

func (f *fakeSubagents) ChatSubagents(_ context.Context, _, backendID, session string) (hub.Subagents, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if backendID != "pc" {
		return hub.Subagents{}, hub.ErrNoSubagentRoutes
	}
	return f.answer(session), nil
}

func (f *fakeSubagents) SetChatSubagents(_ context.Context, _, _, session, choice string) (hub.Subagents, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.choice[session] = choice
	return f.answer(session), nil
}

func putSubagents(t *testing.T, c *http.Client, u, csrf, origin, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, u, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
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

func TestSubagentsAPI(t *testing.T) {
	t.Parallel()
	subs := &fakeSubagents{choice: map[string]string{}}
	e := setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		cfg.Backends[0].Users = []string{userAlice}
		opts.Subagents = subs
	})
	alice := client(t)
	m := login(t, e, alice)
	u := e.url + "/api/sessions/pc/s1/subagents"
	if resp, body := get(t, alice, u); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"choice":"auto"`) {
		t.Fatalf("get = %d %s", resp.StatusCode, body)
	}
	if resp, _ := putSubagents(t, alice, u, "", "", `{"choice":"haiku"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("put without CSRF = %d", resp.StatusCode)
	}
	if resp, body := putSubagents(t, alice, u, m.CSRF, e.url, `{"choice":"haiku"}`); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"choice":"haiku"`) {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, alice, e.url+"/api/sessions/home/s1/subagents"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("backend without the plugin = %d", resp.StatusCode)
	}
}

func TestSubagentsAPIOff(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(cfg *config.Config, _ *web.Options, _ *store.Store) {
		cfg.Backends[0].Users = []string{userAlice}
	})
	alice := client(t)
	login(t, e, alice)
	if resp, _ := get(t, alice, e.url+"/api/sessions/pc/s1/subagents"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no subagents core = %d", resp.StatusCode)
	}
}
