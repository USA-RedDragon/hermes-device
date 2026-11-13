// Package modelstest fakes the setup routes of Hermes's dashboard that
// package models calls, with the answers a real Hermes v0.23.1 gave.
package modelstest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Hermes is a fake Hermes dashboard. It keeps API keys, servers, sign-ins
// and the default model in memory, as Hermes keeps them in its own files.
type Hermes struct {
	mu sync.Mutex
	// Keys maps an API key variable to its value.
	Keys map[string]string
	// Endpoints maps a server id to what was saved.
	Endpoints map[string]map[string]any
	// Model and Provider are the default.
	Model, Provider string
	// SignedIn names the accounts signed in.
	SignedIn map[string]bool
	// Bodies holds every request body Hermes got, in order.
	Bodies []string
	// Paths holds every method and path Hermes got, in order.
	Paths []string
	// Down makes every request fail as if Hermes were away.
	Down bool
	mux  *http.ServeMux
}

// New returns a Hermes with no model.
func New() *Hermes {
	h := &Hermes{Keys: map[string]string{}, Endpoints: map[string]map[string]any{}, SignedIn: map[string]bool{}, mux: http.NewServeMux()}
	h.routes()
	return h
}

// Request implements device.Requester.
func (h *Hermes) Request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	h.mu.Lock()
	down := h.Down
	h.Paths = append(h.Paths, method+" "+path)
	if body != nil {
		h.Bodies = append(h.Bodies, string(body))
	}
	h.mu.Unlock()
	if down {
		return nil, io.ErrUnexpectedEOF
	}
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req := httptest.NewRequestWithContext(ctx, method, path, rd)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// RequestSlow is Request.
func (h *Hermes) RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return h.Request(ctx, method, path, body)
}

// Keys and values the answers repeat.
const (
	kProvider  = "provider"
	kModel     = "model"
	kModels    = "models"
	kDetail    = "detail"
	kMessage   = "message"
	kCategory  = "category"
	kIsCurrent = "is_current"
	openRouter = "openrouter"
	sessionID  = "sess1"
)

func write(w http.ResponseWriter, code int, v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func read(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

// mask is Hermes's redacted_credential_preview.
func mask(v string) string {
	if len(v) < 12 {
		return "«redacted:…»"
	}
	return "«redacted:" + v[:4] + "..." + v[len(v)-4:] + "»"
}

type envVar struct{ provider, label, url string }

func envVars() map[string]envVar {
	return map[string]envVar{
		"OPENROUTER_API_KEY": {openRouter, "OpenRouter", "https://openrouter.ai/keys"},
		"DEEPSEEK_API_KEY":   {"deepseek", "DeepSeek", "https://platform.deepseek.com/api_keys"},
		"GOOGLE_API_KEY":     {"gemini", "Google AI Studio", "https://aistudio.google.com/app/apikey"},
		"LM_API_KEY":         {"lmstudio", "LM Studio", ""},
	}
}

func (h *Hermes) routes() {
	h.mux.HandleFunc("GET /api/env", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := map[string]any{
			"HERMES_SECRET":     map[string]any{"is_set": true, "redacted_value": "«redacted:…»", kCategory: "tool", "is_password": true},
			"DEEPSEEK_BASE_URL": map[string]any{"is_set": false, kProvider: "deepseek", "provider_label": "DeepSeek", kCategory: "provider"},
		}
		for k, v := range envVars() {
			val := h.Keys[k]
			row := map[string]any{"is_set": val != "", "redacted_value": nil, kProvider: v.provider, "provider_label": v.label,
				"provider_primary": true, kCategory: "provider", "is_password": true, "url": v.url}
			if val != "" {
				row["redacted_value"] = mask(val)
			}
			out[k] = row
		}
		write(w, http.StatusOK, out)
	})
	h.mux.HandleFunc("PUT /api/env", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		h.mu.Lock()
		defer h.mu.Unlock()
		key, _ := b["key"].(string)
		h.Keys[key], _ = b["value"].(string)
		write(w, http.StatusOK, map[string]any{"ok": true, "key": key})
	})
	h.mux.HandleFunc("DELETE /api/env", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		h.mu.Lock()
		defer h.mu.Unlock()
		key, _ := b["key"].(string)
		if _, ok := h.Keys[key]; !ok {
			write(w, http.StatusNotFound, map[string]any{kDetail: key + " not found in .env"})
			return
		}
		delete(h.Keys, key)
		write(w, http.StatusOK, map[string]any{"ok": true, "found": true})
	})
	h.mux.HandleFunc("POST /api/providers/validate", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		if v, _ := b["value"].(string); strings.Contains(v, "bad") {
			write(w, http.StatusOK, map[string]any{"ok": false, "reachable": true, kMessage: "That API key was rejected. Double-check it and try again."})
			return
		}
		write(w, http.StatusOK, map[string]any{"ok": true, "reachable": true, kMessage: ""})
	})
	h.mux.HandleFunc("POST /api/providers/custom-endpoints/validate", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		u, _ := b["base_url"].(string)
		if strings.Contains(u, "localhost") {
			write(w, http.StatusOK, map[string]any{"ok": false, "reachable": false, kMessage: "Could not reach " + u + "/models.", kModels: []string{}})
			return
		}
		write(w, http.StatusOK, map[string]any{"ok": true, "reachable": true, kMessage: "", kModels: []string{"qwen2.5:0.5b", "llama3.2:3b"},
			"transport_checked": "chat_completions", "resolved_base_url": strings.TrimSuffix(u, "/v1") + "/v1"})
	})
	h.mux.HandleFunc("POST /api/providers/custom-endpoints", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		h.mu.Lock()
		defer h.mu.Unlock()
		id, _ := b["id"].(string)
		if id == "" {
			id, _ = b["name"].(string)
			id = strings.ToLower(id)
		}
		h.Endpoints[id] = b
		if mk, _ := b["make_default"].(bool); mk {
			h.Model, _ = b[kModel].(string)
			h.Provider = id
		}
		write(w, http.StatusOK, map[string]any{"ok": true, "id": id, "endpoints": []any{}})
	})
	h.mux.HandleFunc("GET /api/providers/custom-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		eps := make([]any, 0, len(h.Endpoints))
		for id, e := range h.Endpoints {
			eps = append(eps, map[string]any{"id": id, "name": e["name"], "base_url": e["base_url"], kModel: e[kModel],
				"has_api_key": e["api_key"] != nil, "api_key_preview": nil, kIsCurrent: h.Provider == id})
		}
		write(w, http.StatusOK, map[string]any{"endpoints": eps})
	})
	h.mux.HandleFunc("DELETE /api/providers/custom-endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		id := r.PathValue("id")
		if _, ok := h.Endpoints[id]; !ok {
			write(w, http.StatusNotFound, map[string]any{kDetail: "custom endpoint not found"})
			return
		}
		delete(h.Endpoints, id)
		write(w, http.StatusOK, map[string]any{"ok": true})
	})
	h.mux.HandleFunc("GET /api/model/options", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		yes := true
		ps := []any{}
		for id, e := range h.Endpoints {
			ps = append(ps, map[string]any{"slug": id, "name": e["name"], kModels: []string{"qwen2.5:0.5b", "llama3.2:3b"}, "authenticated": yes,
				kIsCurrent: h.Provider == id})
		}
		if h.Keys["OPENROUTER_API_KEY"] != "" || h.Provider == openRouter {
			ps = append(ps, map[string]any{"slug": openRouter, "name": "OpenRouter", kModels: []string{"openai/gpt-5"},
				"authenticated": h.Keys["OPENROUTER_API_KEY"] != "", kIsCurrent: h.Provider == openRouter})
		}
		write(w, http.StatusOK, map[string]any{"providers": ps, kModel: h.Model, kProvider: h.Provider})
	})
	h.mux.HandleFunc("POST /api/model/set", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		h.mu.Lock()
		defer h.mu.Unlock()
		m, _ := b[kModel].(string)
		if c, _ := b["confirm_expensive_model"].(bool); strings.Contains(m, "opus") && !c {
			write(w, http.StatusOK, map[string]any{"ok": false, "confirm_required": true, "confirm_message": m + " is a high-priced model."})
			return
		}
		h.Model, _ = b[kModel].(string)
		h.Provider, _ = b[kProvider].(string)
		write(w, http.StatusOK, map[string]any{"ok": true, kProvider: h.Provider, kModel: h.Model})
	})
	h.mux.HandleFunc("GET /api/model/info", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		write(w, http.StatusOK, map[string]any{kModel: h.Model, kProvider: h.Provider, "effective_context_length": 32768})
	})
	h.mux.HandleFunc("GET /api/model/recommended-default", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]any{kProvider: r.URL.Query().Get("provider"), kModel: "openai/gpt-5", "free_tier": nil})
	})
	h.mux.HandleFunc("GET /api/providers/oauth", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		row := func(id, name, flow string) map[string]any {
			return map[string]any{"id": id, "name": name, "flow": flow, "cli_command": "hermes auth add " + id, "disconnectable": true,
				"status": map[string]any{"logged_in": h.SignedIn[id]}}
		}
		write(w, http.StatusOK, map[string]any{"providers": []any{row("nous", "Nous Portal", "device_code"),
			row("openai-codex", "ChatGPT or Codex Subscription", "device_code"), row("qwen-oauth", "Qwen (via Qwen CLI)", "external")}})
	})
	h.mux.HandleFunc("POST /api/providers/oauth/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"session_id": sessionID, "flow": "device_code", "user_code": "WXYZ-1234",
			"verification_url": "https://example.com/device", "expires_in": 900, "poll_interval": 5})
	})
	h.mux.HandleFunc("GET /api/providers/oauth/{id}/poll/{session}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("session") != sessionID {
			write(w, http.StatusNotFound, map[string]any{kDetail: "Session not found or expired"})
			return
		}
		write(w, http.StatusOK, map[string]any{"session_id": sessionID, "status": "pending"})
	})
	h.mux.HandleFunc("DELETE /api/providers/oauth/sessions/{session}", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"ok": true})
	})
	h.mux.HandleFunc("DELETE /api/providers/oauth/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.SignedIn[r.PathValue("id")] {
			write(w, http.StatusConflict, map[string]any{kDetail: "No stored credentials were removed"})
			return
		}
		delete(h.SignedIn, r.PathValue("id"))
		write(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// Seen reports whether Hermes got a request whose method and path start
// with prefix.
func (h *Hermes) Seen(prefix string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.Paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
