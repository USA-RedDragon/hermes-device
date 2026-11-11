package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/memory"
	"github.com/USA-RedDragon/hermes-hub/internal/memory/memorytest"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const (
	honchoKey    = "honcho-jwt-for-tests" //nolint:gosec // a test value, not a credential
	fieldContent = "content"
	fieldIDs     = "ids"
)

type lateAccess struct{ e **hubEnv }

func (l lateAccess) Allowed(ctx context.Context, user, backendID string) error {
	return (*l.e).svc.Allowed(ctx, user, backendID)
}

func (l lateAccess) Backends(ctx context.Context, user string) ([]hub.Backend, error) {
	return (*l.e).svc.Backends(ctx, user)
}

type memoryEnv struct {
	*hubEnv
	pc, home *memorytest.Plugin
	honcho   *memorytest.Honcho
}

// setupMemory gives backend pc (alice's) notes and backend home (the
// family group's, so bob's) notes and Honcho.
func setupMemory(t *testing.T) *memoryEnv {
	t.Helper()
	notes := func(entry string) hub.Notes {
		return hub.Notes{Targets: map[string]hub.NoteStore{
			hub.TargetMemory: {File: "/data/memories/MEMORY.md", Limit: 2200, Enabled: true, Entries: []string{entry}},
			hub.TargetUser:   {File: "/data/memories/USER.md", Limit: 1375, Enabled: true},
		}, Pending: []hub.PendingNote{{ID: "p1", Target: hub.TargetMemory, Changes: []hub.NoteChange{{Action: "add", Content: "<img src=x onerror=alert(1)>"}}}}}
	}
	pc, home := memorytest.NewPlugin(notes("alice's note")), memorytest.NewPlugin(notes("bob's note"))
	t.Cleanup(pc.Close)
	t.Cleanup(home.Close)
	h := memorytest.NewHoncho("bob", honchoKey)
	t.Cleanup(h.Close)
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		cfg.Backends[1].Honcho = config.Honcho{URL: h.URL(), Workspace: "bob", UserPeer: userBob, APIKey: honchoKey}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		m, err := memory.New(memory.Options{Access: lateAccess{&e}, Backends: cfg.Backends,
			Requesters: map[string]device.Requester{"pc": pc, backendHome: home}})
		if err != nil {
			t.Fatal(err)
		}
		opts.Memory = m
	})
	return &memoryEnv{hubEnv: e, pc: pc, home: home, honcho: h}
}

func TestMemoryAPI(t *testing.T) {
	t.Parallel()
	e := setupMemory(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	_, body := get(t, c, e.url+"/api/me")
	if !strings.Contains(body, `"memory":true`) {
		t.Fatalf("me = %s", body)
	}
	var bs []hub.MemoryBackend
	if code := apiJSON(t, c, http.MethodGet, e.url+"/api/memory", m.CSRF, nil, &bs); code != http.StatusOK || len(bs) != 1 ||
		bs[0].ID != "pc" || bs[0].Pending != 1 || !bs[0].Notes || bs[0].Learned {
		t.Fatalf("alice memory = %d %+v", code, bs)
	}
	var n hub.Notes
	if code := apiJSON(t, c, http.MethodGet, e.url+"/api/memory/pc/notes", "", nil, &n); code != http.StatusOK ||
		n.Targets[hub.TargetMemory].Entries[0] != "alice's note" || n.Pending[0].Changes[0].Content != "<img src=x onerror=alert(1)>" {
		t.Fatalf("notes = %d %+v", code, n)
	}
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/pc/notes/memory/add", m.CSRF, map[string]string{fieldContent: "Short answers."}, &n); code != http.StatusOK ||
		len(n.Targets[hub.TargetMemory].Entries) != 2 {
		t.Fatalf("add = %d %+v", code, n)
	}
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/pc/pending/p1/approve", m.CSRF, map[string]string{}, &n); code != http.StatusOK || len(n.Pending) != 0 {
		t.Fatalf("approve = %d %+v", code, n)
	}
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/pc/approval", m.CSRF, map[string]bool{"enabled": true}, &n); code != http.StatusOK || !n.Approval {
		t.Fatalf("approval = %d %+v", code, n)
	}
	var errm map[string]any
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/pc/approval", m.CSRF, map[string]string{}, &errm); code != http.StatusBadRequest {
		t.Fatalf("approval without enabled = %d", code)
	}
	if code := apiJSON(t, c, http.MethodGet, e.url+"/api/memory/pc/learned", "", nil, &errm); code != http.StatusNotFound {
		t.Fatalf("learned on pc = %d", code)
	}
}

func TestMemoryAccess(t *testing.T) {
	t.Parallel()
	e := setupMemory(t)
	c := client(t)
	e.is.as("sub-bob", userBob, "family")
	m := login(t, e.hubEnv, c)
	var bs []hub.MemoryBackend
	if code := apiJSON(t, c, http.MethodGet, e.url+"/api/memory", "", nil, &bs); code != http.StatusOK || len(bs) != 1 || bs[0].ID != backendHome || !bs[0].Learned {
		t.Fatalf("bob memory = %d %+v", code, bs)
	}
	before := len(e.pc.Requests())
	var errm map[string]any
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/memory/pc/notes"},
		{http.MethodPost, "/api/memory/pc/notes/memory/add"},
		{http.MethodPost, "/api/memory/pc/notes/user/replace"},
		{http.MethodPost, "/api/memory/pc/pending/p1/approve"},
		{http.MethodPost, "/api/memory/pc/approval"},
	} {
		body := map[string]any{fieldContent: "x", "enabled": true}
		if code := apiJSON(t, c, r.method, e.url+r.path, m.CSRF, body, &errm); code != http.StatusForbidden {
			t.Errorf("%s %s = %d", r.method, r.path, code)
		}
	}
	if after := len(e.pc.Requests()); after != before {
		t.Fatalf("alice's Hermes got %d requests from bob", after-before)
	}
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/home/notes/memory/add", "", map[string]string{fieldContent: "x"}, &errm); code != http.StatusForbidden {
		t.Fatalf("add without CSRF = %d", code)
	}
	resp, _ := post(t, c, e.url+"/api/memory/home/learned/delete", m.CSRF, "https://evil.example.com", map[string]any{fieldIDs: []string{"c1"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin delete = %d", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.url+"/api/memory/home/learned/export", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site export = %d", r2.StatusCode)
	}
	for _, r := range e.home.Requests() {
		if strings.Contains(r.Path, "/add") {
			t.Fatalf("a refused add reached Hermes: %+v", r)
		}
	}
	if n := len(e.honcho.Requests()); n != 0 {
		t.Fatalf("honcho got %d requests from refused calls", n)
	}
}

func TestLearnedAPI(t *testing.T) {
	t.Parallel()
	e := setupMemory(t)
	c := client(t)
	e.is.as("sub-bob", userBob, "family")
	m := login(t, e.hubEnv, c)
	a := e.honcho.Add("hermes", userBob, "Prefers restic.", "s1")
	e.honcho.Add(userBob, userBob, "<script>alert(1)</script>", "")
	other := e.honcho.Add("hermes", "hermes", "Runs on a GPU.", "")
	var p hub.LearnedPage
	if code := apiJSON(t, c, http.MethodGet, e.url+"/api/memory/home/learned?view=hermes", "", nil, &p); code != http.StatusOK ||
		len(p.Items) != 1 || p.Items[0].ID != a || p.Items[0].SessionID != "s1" {
		t.Fatalf("hermes view = %d %+v", code, p)
	}
	var info hub.LearnedInfo
	if code := apiJSON(t, c, http.MethodGet, e.url+"/api/memory/home/learned/info", "", nil, &info); code != http.StatusOK || info.Total != 2 {
		t.Fatalf("info = %d %+v", code, info)
	}
	var res map[string]any
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/home/learned/delete", m.CSRF, map[string]any{fieldIDs: []string{other}}, &res); code != http.StatusNotFound {
		t.Fatalf("delete another peer's fact = %d %v", code, res)
	}
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/home/learned/delete", m.CSRF, map[string]any{fieldIDs: []string{a}}, &res); code != http.StatusOK || res["deleted"] != float64(1) {
		t.Fatalf("delete = %d %v", code, res)
	}
	var added hub.Conclusion
	if code := apiJSON(t, c, http.MethodPost, e.url+"/api/memory/home/learned", m.CSRF, map[string]string{fieldContent: "Has a garage."}, &added); code != http.StatusCreated || added.View != hub.ViewHermes {
		t.Fatalf("add = %d %+v", code, added)
	}
	resp, body := get(t, c, e.url+"/api/memory/home/learned/export")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("export = %d %v", resp.StatusCode, resp.Header)
	}
	var out []hub.Conclusion
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out) != 2 {
		t.Fatalf("export body = %v %s", err, body)
	}
	if strings.Contains(body, honchoKey) {
		t.Fatal("the export holds the Honcho key")
	}
	for _, r := range e.honcho.Requests() {
		if r.Auth != "Bearer "+honchoKey {
			t.Fatalf("honcho request without the key: %+v", r)
		}
	}
}
