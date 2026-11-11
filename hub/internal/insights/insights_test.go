package insights_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/insights"
)

const (
	alice        = "alice"
	bob          = "bob"
	searchPath   = "/api/plugins/device/search"
	keyMetric    = "metric"
	keySessionID = "session_id"
	keyValue     = "value"
)

// access lets alice use "pc" and "home", and bob only "bobs".
type access struct{}

func backends(user string) []hub.Backend {
	return map[string][]hub.Backend{
		alice: {{ID: "pc", Name: "PC"}, {ID: "home", Name: "Home"}},
		bob:   {{ID: "bobs", Name: "Bob's"}},
	}[user]
}

func (access) Backends(_ context.Context, user string) ([]hub.Backend, error) {
	return backends(user), nil
}

func (access) Allowed(_ context.Context, user, id string) error {
	for _, b := range backends(user) {
		if b.ID == id {
			return nil
		}
	}
	return hub.ErrForbidden
}

// requester serves plugin routes from a handler.
type requester struct{ h http.Handler }

func (r requester) Request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Result(), nil
}

func jsonHandler(routes map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
			return
		}
		if code, ok := v.(int); ok {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"busy","message":"busy"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	})
}

type observer struct{}

func (observer) Observed(ctx context.Context, user, id string) (hub.Observed, time.Time, error) {
	if err := (access{}).Allowed(ctx, user, id); err != nil {
		return hub.Observed{}, time.Time{}, err
	}
	return hub.Observed{Limits: []hub.ProviderLimits{{Provider: "openrouter"}}}, time.Unix(100, 0), nil
}

type backlogs struct{}

func (backlogs) Backlog(_ context.Context, _, id string) (hub.Backlog, error) {
	if id == "pc" {
		return hub.Backlog{}, hub.ErrNoMemory
	}
	return hub.Backlog{Total: 5, Pending: 2}, nil
}

func newService(t *testing.T, prom http.Handler) *insights.Service {
	t.Helper()
	pc := jsonHandler(map[string]any{
		searchPath: map[string]any{"results": []any{map[string]any{keySessionID: "s1", "title": "Trip",
			"last_active": 1.7e9, "hits": []any{map[string]any{"row_id": 4, keySessionID: "s0", "role": "user", "snippet": "a >>>lisbon<<< b", "timestamp": 1.7e9}}}}},
		"/api/plugins/device/usage": map[string]any{"since": 1.6e9, "sessions": []any{map[string]any{keySessionID: "s0", "chat_id": "s1",
			"title": "Trip", "model": "qwen", "provider": "ollama", "last_active": 1.7e9, "input": 10, "output": 2, "calls": 1, "cost_usd": -1}},
			"models": []any{map[string]any{"model": "qwen", "provider": "ollama", "input": 10, "output": 2, "calls": 1}}},
	})
	home := jsonHandler(map[string]any{searchPath: http.StatusTooManyRequests})
	bobs := jsonHandler(map[string]any{searchPath: map[string]any{"results": []any{map[string]any{keySessionID: "bob-secret"}}}})
	opts := insights.Options{Access: access{}, Observer: observer{}, Backlogs: backlogs{},
		Requesters: map[string]device.Requester{"pc": requester{pc}, "home": requester{home}, "bobs": requester{bobs}}}
	if prom != nil {
		srv := httptest.NewServer(prom)
		t.Cleanup(srv.Close)
		opts.GPU = insights.NewGPU(srv.URL, srv.Client())
	}
	return insights.New(opts)
}

func TestSearch(t *testing.T) {
	t.Parallel()
	s := newService(t, nil)
	ctx := context.Background()
	r, err := s.Search(ctx, alice, "  lisbon ")
	if err != nil || len(r.Results) != 1 {
		t.Fatalf("search = %+v, %v", r, err)
	}
	got := r.Results[0]
	if got.BackendID != "pc" || got.SessionID != "s1" || got.Hits[0].RowID != 4 || got.Hits[0].SessionID != "s0" ||
		got.Hits[0].Snippet != "a >>>lisbon<<< b" || got.LastActive.IsZero() {
		t.Fatalf("result = %+v", got)
	}
	if !strings.Contains(r.Errors["Home"], "busy") {
		t.Fatalf("errors = %v", r.Errors)
	}
	for _, res := range r.Results {
		if res.SessionID == "bob-secret" {
			t.Fatal("alice saw bob's chat")
		}
	}
	for _, q := range []string{"", "   ", strings.Repeat("x", 201)} {
		if _, err := s.Search(ctx, alice, q); !errors.Is(err, hub.ErrBadQuery) {
			t.Fatalf("query %q = %v", q, err)
		}
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	prom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		res := []any{map[string]any{keyMetric: map[string]string{}, keyValue: []any{1, "120"}}}
		if strings.Contains(q, "by (class)") {
			res = []any{map[string]any{keyMetric: map[string]string{"class": "high"}, keyValue: []any{1, "60"}},
				map[string]any{keyMetric: map[string]string{"class": "low"}, keyValue: []any{1, "30"}}}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": res}}); err != nil {
			t.Error(err)
		}
	})
	s := newService(t, prom)
	ctx := context.Background()
	u, err := s.Usage(ctx, alice, "pc", 500)
	if err != nil || u.Days != insights.MaxDays || len(u.Sessions) != 1 || u.Sessions[0].ChatID != "s1" || u.Sessions[0].Input != 10 ||
		u.Sessions[0].CostUSD != 0 || len(u.Models) != 1 || u.TokensError != "" {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	if u.Backlog != nil || u.BacklogError != "" || len(u.Observed.Limits) != 1 || u.ObservedSince.IsZero() {
		t.Fatalf("usage extras = %+v", u)
	}
	if len(u.GPU) != 3 || u.GPU[0].Busy != 120 || u.GPU[0].People != 60 || u.GPU[0].Background != 30 || u.GPU[0].Requests != 120 {
		t.Fatalf("gpu = %+v", u.GPU)
	}
	u, err = s.Usage(ctx, alice, "home", 7)
	if err != nil || u.Backlog == nil || u.Backlog.Pending != 2 || !strings.Contains(u.TokensError, "too old") {
		t.Fatalf("home usage = %+v, %v", u, err)
	}
	if _, err := s.Usage(ctx, alice, "bobs", 7); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("alice on bob's backend = %v", err)
	}
	failing := newService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	if u, err := failing.Usage(ctx, alice, "pc", 7); err != nil || u.GPUError == "" || u.GPU != nil {
		t.Fatalf("failing GPU = %+v, %v", u, err)
	}
}

func TestGPUCache(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if err := json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	g := insights.NewGPU(srv.URL, srv.Client())
	for range 3 {
		if _, err := g.Windows(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 9 {
		t.Fatalf("queries = %d, want 9 (one load, then the cache)", calls)
	}
}
