// Package agentcfgtest fakes the agent dashboard routes package agentcfg
// calls, with answers shaped as a real agent (Hermes v0.23.1 with the
// Talaria patches) gives them.
package agentcfgtest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Agent is a fake agent. It keeps its settings in memory.
type Agent struct {
	mu sync.Mutex
	// Config is the agent's config, as GET /api/config answers it.
	Config map[string]any
	Soul   string
	// Toolsets, Skills and Servers are what the lists answer.
	Toolsets []map[string]any
	Skills   []map[string]any
	Servers  []map[string]any
	// Live is what applied_live answers, when dashboard.apply_to_live_chats is on.
	Live bool
	// Paths and Bodies record every request.
	Paths  []string
	Bodies []string
	// Down makes every request fail.
	Down bool
	// Archive is the backup archive's content.
	Archive []byte
	mux     *http.ServeMux
}

// New returns an agent with a few toolsets, skills and one connection.
func New() *Agent {
	a := &Agent{
		Config: map[string]any{"approvals": map[string]any{"mode": "smart"}, "compression": map[string]any{kEnabled: true, "threshold": 0.5},
			"timezone": "", "dashboard": map[string]any{"apply_to_live_chats": true}},
		Soul: "You are helpful.",
		Toolsets: []map[string]any{
			{kName: "web", kLabel: "Web Search & Scraping", kDesc: "web_search, web_extract", kEnabled: true, kConfigured: true},
			{kName: "terminal", kLabel: "Terminal & Processes", kDesc: "terminal, process", kEnabled: true, kConfigured: true},
			{kName: "image_gen", kLabel: "Image Generation", kDesc: "image_generate", kEnabled: false, kConfigured: false},
		},
		Skills: []map[string]any{
			{kName: "github", kDesc: "Work with GitHub", kEnabled: true, "provenance": "bundled"},
			{kName: "weather-brief", kDesc: "Short forecasts", kEnabled: false, "provenance": "hub"},
		},
		Servers: []map[string]any{{kName: "home-calendar", "transport": "http", "url": "https://calendar.example/mcp", kEnabled: true, "source": "config"}},
		Live:    true, Archive: []byte("PK-agent-archive"), mux: http.NewServeMux(),
	}
	a.routes()
	return a
}

// Request implements device.Requester.
func (a *Agent) Request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	a.mu.Lock()
	a.Paths = append(a.Paths, method+" "+path)
	if body != nil {
		a.Bodies = append(a.Bodies, string(body))
	}
	down := a.Down
	a.mu.Unlock()
	if down {
		return nil, io.ErrUnexpectedEOF
	}
	req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// Seen reports whether a request with this method and path prefix came.
func (a *Agent) Seen(prefix string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.Paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func write(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

const (
	kEnabled    = "enabled"
	kConfigured = "configured"
	kDesc       = "description"
	kLabel      = "label"
	kName       = "name"
)

func (a *Agent) live(out map[string]any) map[string]any {
	if d, _ := a.Config["dashboard"].(map[string]any); d != nil && d["apply_to_live_chats"] == true {
		out["applied_live"] = a.Live
	}
	return out
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if m, ok := v.(map[string]any); ok {
			d, _ := dst[k].(map[string]any)
			if d == nil {
				d = map[string]any{}
				dst[k] = d
			}
			merge(d, m)
			continue
		}
		dst[k] = v
	}
}

func (a *Agent) routes() {
	m := a.mux
	lock := func(h func(w http.ResponseWriter, r *http.Request)) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			h(w, r)
		}
	}
	m.HandleFunc("GET /api/config", lock(func(w http.ResponseWriter, _ *http.Request) { write(w, a.Config) }))
	m.HandleFunc("PUT /api/config", lock(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Config map[string]any `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		merge(a.Config, b.Config)
		write(w, map[string]any{"ok": true})
	}))
	m.HandleFunc("GET /api/profiles/default/soul", lock(func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"content": a.Soul, "exists": a.Soul != ""})
	}))
	m.HandleFunc("PUT /api/profiles/default/soul", lock(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		a.Soul = b.Content
		write(w, a.live(map[string]any{"ok": true}))
	}))
	m.HandleFunc("GET /api/tools/toolsets", lock(func(w http.ResponseWriter, _ *http.Request) { write(w, a.Toolsets) }))
	m.HandleFunc("PUT /api/tools/toolsets/{name}", lock(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, t := range a.Toolsets {
			if t[kName] == r.PathValue(kName) {
				t[kEnabled] = b.Enabled
				write(w, a.live(map[string]any{"ok": true, kName: t[kName], kEnabled: b.Enabled}))
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
		write(w, map[string]any{"detail": "Unknown toolset: " + r.PathValue(kName)})
	}))
	m.HandleFunc("GET /api/skills", lock(func(w http.ResponseWriter, _ *http.Request) { write(w, a.Skills) }))
	m.HandleFunc("PUT /api/skills/toggle", lock(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, s := range a.Skills {
			if s[kName] == b.Name {
				s[kEnabled] = b.Enabled
			}
		}
		write(w, a.live(map[string]any{"ok": true, kName: b.Name, kEnabled: b.Enabled}))
	}))
	m.HandleFunc("GET /api/mcp/servers", lock(func(w http.ResponseWriter, _ *http.Request) { write(w, map[string]any{"servers": a.Servers}) }))
	m.HandleFunc("POST /api/mcp/servers", lock(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		a.Servers = append(a.Servers, map[string]any{kName: b[kName], "transport": "http", "url": b["url"], kEnabled: true, "source": "config"})
		write(w, a.live(map[string]any{kName: b[kName]}))
	}))
	m.HandleFunc("DELETE /api/mcp/servers/{name}", lock(func(w http.ResponseWriter, r *http.Request) {
		out := a.Servers[:0]
		for _, s := range a.Servers {
			if s[kName] != r.PathValue(kName) {
				out = append(out, s)
			}
		}
		a.Servers = out
		write(w, a.live(map[string]any{"ok": true}))
	}))
	m.HandleFunc("PUT /api/mcp/servers/{name}/enabled", lock(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, s := range a.Servers {
			if s[kName] == r.PathValue(kName) {
				s[kEnabled] = b.Enabled
			}
		}
		write(w, a.live(map[string]any{"ok": true, kName: r.PathValue(kName), kEnabled: b.Enabled}))
	}))
	m.HandleFunc("GET /api/status", lock(func(w http.ResponseWriter, _ *http.Request) { write(w, map[string]any{"version": "0.23.1"}) }))
	m.HandleFunc("GET /api/system/stats", lock(func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"process": map[string]any{"rss_mb": 312.4}})
	}))
	m.HandleFunc("GET /api/logs", lock(func(w http.ResponseWriter, r *http.Request) {
		lines := []string{"2026-11-13 10:41:02 INFO chat started", "2026-11-13 10:44:30 WARNING provider slow"}
		if r.URL.Query().Get("level") == "WARNING" {
			lines = lines[1:]
		}
		write(w, map[string]any{"file": "agent", "lines": lines})
	}))
	m.HandleFunc("POST /api/ops/doctor", lock(func(w http.ResponseWriter, _ *http.Request) { write(w, map[string]any{"ok": true, kName: "doctor"}) }))
	m.HandleFunc("POST /api/ops/backup", lock(func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"ok": true, kName: "backup", "archive": "/opt/data/backups/hermes-backup-2026-11-13-104500-0123abcd.zip"})
	}))
	m.HandleFunc("GET /api/actions/{name}/status", lock(func(w http.ResponseWriter, r *http.Request) {
		lines := []string{"Checking...", "✓ config ok", "✓ model reachable", "⚠ data folder 91% full"}
		write(w, map[string]any{kName: r.PathValue(kName), "running": false, "exit_code": 0, "lines": lines})
	}))
	m.HandleFunc("GET /api/ops/backup/download", lock(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(a.Archive)
	}))
	m.HandleFunc("POST /api/dashboard/apply-live", lock(func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"ok": true, "applied_live": a.Live})
	}))
}
