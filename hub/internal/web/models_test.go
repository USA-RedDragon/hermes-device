package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/models"
	"github.com/USA-RedDragon/talaria/hub/internal/models/modelstest"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// canary stands in for an API key that no answer, log or file of the hub
// may hold.
const canary = "sk-or-v1-0123456789abcdefCANARYVALUE9876"

const (
	provOpenRouter = "openrouter"
	varOpenRouter  = "OPENROUTER_API_KEY"
	nameOllama     = "Ollama"
	ollamaV1       = "http://ollama:11434/v1"
	fieldProv      = "provider"
	fieldConfirm   = "confirm"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type modelsEnv struct {
	*hubEnv
	pc, home *modelstest.Hermes
	logs     *syncBuf
}

func setupModels(t *testing.T) *modelsEnv {
	t.Helper()
	pc, home := modelstest.New(), modelstest.New()
	logs := &syncBuf{}
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		opts.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		opts.Models = models.New(models.Options{Access: lateAccess{&e}, Backends: cfg.Backends,
			Requesters: map[string]device.Requester{"pc": pc, backendHome: home}})
	})
	return &modelsEnv{hubEnv: e, pc: pc, home: home, logs: logs}
}

// call sends one request with any method and decodes the answer into a
// map.
func call(t *testing.T, c *http.Client, method, u, csrf string, body any) (int, map[string]any, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, u, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(text, &out)
	return resp.StatusCode, out, string(text)
}

func TestModelsConnectFlow(t *testing.T) {
	t.Parallel()
	e := setupModels(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	base := e.url + "/api/models/pc"
	modelsCatalog(t, e, c, base)
	modelsServer(t, c, base, m.CSRF)
	modelsKey(t, e, c, base, m.CSRF)
	modelsSignIn(t, e, c, base, m.CSRF)
	modelsNoLeak(t, e)
}

func modelsCatalog(t *testing.T, e *modelsEnv, c *http.Client, base string) {
	t.Helper()
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"models_admin":true`) {
		t.Fatalf("alice is the admin: %s", body)
	}
	if code, st, _ := call(t, c, http.MethodGet, base+"/status", "", nil); code != http.StatusOK || st["ready"] != false || st["admin"] != true {
		t.Fatalf("status = %d %v", code, st)
	}

	code, cat, text := call(t, c, http.MethodGet, base+"/catalog", "", nil)
	if code != http.StatusOK || !strings.Contains(text, `"key_env":"OPENROUTER_API_KEY"`) || !strings.Contains(text, `"key_env":"DEEPSEEK_API_KEY"`) ||
		strings.Contains(text, "LM_API_KEY") || strings.Contains(text, "HERMES_SECRET") || strings.Contains(text, "BASE_URL") {
		t.Fatalf("catalog = %d %s", code, text)
	}
	keys, _ := cat["keys"].([]any)
	if first, _ := keys[0].(map[string]any); first[fieldProv] != provOpenRouter || first["checked"] != true || first["common"] != true {
		t.Fatalf("the common providers lead, openrouter is checked: %v", keys)
	}
	if !strings.Contains(text, `"id":"nous","name":"Nous Portal","signed_in":false,"here":true`) || !strings.Contains(text, `"id":"qwen-oauth","name":"Qwen (via Qwen CLI)","signed_in":false,"here":false`) {
		t.Fatalf("accounts = %s", text)
	}

}

func modelsServer(t *testing.T, c *http.Client, base, csrf string) {
	t.Helper()
	// Own server: Hermes's own messages come through as they are.
	if code, r, _ := call(t, c, http.MethodPost, base+"/endpoints/test", csrf, map[string]any{"name": nameOllama, "base_url": "http://localhost:11434/v1"}); code != http.StatusOK ||
		r["message"] != "Could not reach http://localhost:11434/v1/models." || r["reachable"] != false {
		t.Fatalf("test localhost = %d %v", code, r)
	}
	if code, r, _ := call(t, c, http.MethodPost, base+"/endpoints/test", csrf, map[string]any{"name": nameOllama, "base_url": "http://ollama:11434"}); code != http.StatusOK ||
		r["resolved_base_url"] != ollamaV1 {
		t.Fatalf("test = %d %v", code, r)
	}
	if code, _, text := call(t, c, http.MethodPost, base+"/endpoints", csrf, map[string]any{"name": nameOllama, "base_url": ollamaV1}); code != http.StatusBadRequest {
		t.Fatalf("save with no model = %d %s", code, text)
	}
	if code, r, _ := call(t, c, http.MethodPost, base+"/endpoints", csrf, map[string]any{"name": nameOllama, "base_url": ollamaV1,
		keyModel: "qwen2.5:0.5b", "make_default": true}); code != http.StatusOK || r["id"] != "ollama" {
		t.Fatalf("save = %d %v", code, r)
	}
	if code, st, _ := call(t, c, http.MethodGet, base+"/status", "", nil); code != http.StatusOK || st["ready"] != true || st["model"] != "qwen2.5:0.5b" {
		t.Fatalf("status after save = %d %v", code, st)
	}

}

func modelsKey(t *testing.T, e *modelsEnv, c *http.Client, base, csrf string) {
	t.Helper()
	// API key: a rejected key is Hermes's words; a good one is saved in
	// Hermes and nowhere in the hub.
	if code, r, _ := call(t, c, http.MethodPost, base+"/keys/test", csrf, map[string]string{"key": varOpenRouter, "value": "sk-bad"}); code != http.StatusOK ||
		r["message"] != "That API key was rejected. Double-check it and try again." {
		t.Fatalf("bad key = %d %v", code, r)
	}
	if code, r, _ := call(t, c, http.MethodPost, base+"/keys/test", csrf, map[string]string{"key": "DEEPSEEK_API_KEY", "value": "sk-ds"}); code != http.StatusOK ||
		r["ok"] != true || r["reachable"] != false {
		t.Fatalf("a key Hermes cannot check = %d %v", code, r)
	}
	if e.pc.Seen("POST /api/providers/validate") && strings.Contains(strings.Join(e.pc.Bodies, "\n"), "sk-ds") {
		t.Fatal("a key with no probe went to Hermes's probe")
	}
	if code, r, _ := call(t, c, http.MethodPost, base+"/keys/test", csrf, map[string]string{"key": "GOOGLE_API_KEY", "value": "AIza-good"}); code != http.StatusOK || r["reachable"] != true {
		t.Fatalf("Google AI Studio is checked through its alias = %d %v", code, r)
	}
	code, r, text := call(t, c, http.MethodPut, base+"/keys", csrf, map[string]string{"key": varOpenRouter, "value": canary})
	if code != http.StatusOK || strings.Contains(text, canary) || e.pc.Keys[varOpenRouter] != canary {
		t.Fatalf("save key = %d %v", code, r)
	}
	code, _, text = call(t, c, http.MethodGet, base+"/connected", "", nil)
	if code != http.StatusOK || strings.Contains(text, canary) || !strings.Contains(text, `"preview":"sk-o...9876"`) || !strings.Contains(text, `"id":"ollama"`) {
		t.Fatalf("connected = %d %s", code, text)
	}
	code, r, _ = call(t, c, http.MethodPost, base+"/default", csrf, map[string]any{fieldProv: provOpenRouter, keyModel: "anthropic/claude-opus-4.1"})
	if code != http.StatusOK || r["confirm_required"] != true || r["confirm_message"] != "anthropic/claude-opus-4.1 is a high-priced model." {
		t.Fatalf("costly default = %d %v", code, r)
	}
	if code, r, _ = call(t, c, http.MethodPost, base+"/default", csrf, map[string]any{fieldProv: provOpenRouter, keyModel: "anthropic/claude-opus-4.1", fieldConfirm: true}); code != http.StatusOK || r["ok"] != true {
		t.Fatalf("confirmed default = %d %v", code, r)
	}

	modelsKeyGuard(t, e, c, base, csrf)
}

func modelsKeyGuard(t *testing.T, e *modelsEnv, c *http.Client, base, csrf string) {
	t.Helper()
	// Only provider key variables may be written or removed.
	for _, k := range []string{"HERMES_SECRET", "DEEPSEEK_BASE_URL", "LM_API_KEY", "lower_case"} {
		if code, r, _ := call(t, c, http.MethodPut, base+"/keys", csrf, map[string]string{"key": k, "value": "x"}); code != http.StatusBadRequest {
			t.Fatalf("save %s = %d %v", k, code, r)
		}
		if code, _, _ := call(t, c, http.MethodDelete, base+"/keys/"+k, csrf, nil); code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Fatalf("remove %s = %d", k, code)
		}
	}
	if _, ok := e.pc.Keys["HERMES_SECRET"]; ok || e.pc.Seen("DELETE /api/env") {
		t.Fatal("Hermes got a write of a variable that is no provider key")
	}
	if code, _, _ := call(t, c, http.MethodDelete, base+"/keys/OPENROUTER_API_KEY", csrf, nil); code != http.StatusOK || e.pc.Keys[varOpenRouter] != "" {
		t.Fatalf("remove key = %d", code)
	}
	if code, r, _ := call(t, c, http.MethodDelete, base+"/keys/DEEPSEEK_API_KEY", csrf, nil); code != http.StatusNotFound || r["error"] != "DEEPSEEK_API_KEY not found in .env" {
		t.Fatalf("remove a key that is not there = %d %v", code, r)
	}
	if code, _, _ := call(t, c, http.MethodDelete, base+"/endpoints/ollama", csrf, nil); code != http.StatusOK {
		t.Fatalf("remove server = %d", code)
	}
}

func modelsSignIn(t *testing.T, e *modelsEnv, c *http.Client, base, csrf string) {
	t.Helper()

	// Sign-in: the code shows; a session Hermes forgot reads as expired.
	if code, r, _ := call(t, c, http.MethodPost, base+"/signin/nous", csrf, nil); code != http.StatusOK || r["user_code"] != "WXYZ-1234" {
		t.Fatalf("start = %d %v", code, r)
	}
	if code, r, _ := call(t, c, http.MethodPost, base+"/signin/qwen-oauth", csrf, nil); code != http.StatusBadRequest ||
		!strings.Contains(errText(r), "hermes auth add qwen-oauth") {
		t.Fatalf("start of a server-only account = %d %v", code, r)
	}
	if code, r, _ := call(t, c, http.MethodGet, base+"/signin/nous/sess1", "", nil); code != http.StatusOK || r["status"] != "pending" {
		t.Fatalf("poll = %d %v", code, r)
	}
	if code, r, _ := call(t, c, http.MethodGet, base+"/signin/nous/gone", "", nil); code != http.StatusOK || r["status"] != "expired" {
		t.Fatalf("poll of a forgotten session = %d %v", code, r)
	}
	if code, _, _ := call(t, c, http.MethodDelete, base+"/signin/nous/sess1", csrf, nil); code != http.StatusOK || !e.pc.Seen("DELETE /api/providers/oauth/sessions/sess1") {
		t.Fatalf("cancel = %d", code)
	}
	if code, r, _ := call(t, c, http.MethodDelete, base+"/signin/nous", csrf, nil); code != http.StatusConflict || r["error"] != "No stored credentials were removed" {
		t.Fatalf("sign out of nothing = %d %v", code, r)
	}

	// A Hermes that is away.
	e.pc.Down = true
	if code, r, _ := call(t, c, http.MethodGet, base+"/catalog", "", nil); code != http.StatusBadGateway || !strings.HasPrefix(errText(r), "Could not reach Hermes.") {
		t.Fatalf("away = %d %v", code, r)
	}
	e.pc.Down = false

}

func modelsNoLeak(t *testing.T, e *modelsEnv) {
	t.Helper()
	// The key is nowhere in the hub: not in its log, not in its database.
	if strings.Contains(e.logs.String(), canary) {
		t.Fatal("the log holds the key")
	}
	if strings.Contains(strings.Join(e.home.Bodies, "\n"), canary) {
		t.Fatal("another backend got the key")
	}
	dir := filepath.Dir(e.db)
	files, err := os.ReadDir(dir)
	if err != nil || len(files) == 0 {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		if bytes.Contains(b, []byte(canary)) {
			t.Fatalf("%s holds the key", f.Name())
		}
	}
}

func TestModelsAdminOnly(t *testing.T) {
	t.Parallel()
	e := setupModels(t)
	bob := client(t)
	e.is.as("sub-bob", userBob, "family")
	mb := login(t, e.hubEnv, bob)
	if _, body := get(t, bob, e.url+"/api/me"); !strings.Contains(body, `"models_admin":false`) {
		t.Fatalf("bob is no admin: %s", body)
	}
	home := e.url + "/api/models/" + backendHome
	if code, st, _ := call(t, bob, http.MethodGet, home+"/status", "", nil); code != http.StatusOK || st["ready"] != false || st["admin"] != false {
		t.Fatalf("bob's status = %d %v", code, st)
	}
	if code, _, _ := call(t, bob, http.MethodGet, e.url+"/api/models/pc/status", "", nil); code < 400 {
		t.Fatalf("bob reads a backend that is not his: %d", code)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/catalog"}, {http.MethodGet, "/connected"}, {http.MethodGet, "/options"}, {http.MethodPost, "/endpoints/test"},
		{http.MethodPost, "/endpoints"}, {http.MethodPut, "/keys"}, {http.MethodPost, "/keys/test"}, {http.MethodDelete, "/keys/OPENROUTER_API_KEY"},
		{http.MethodPost, "/signin/nous"}, {http.MethodPost, "/default"}, {http.MethodDelete, "/endpoints/x"},
	} {
		body := map[string]string{"key": varOpenRouter, "value": canary, "base_url": "http://x", keyModel: "m", "name": "n"}
		if code, out, _ := call(t, bob, r.method, home+r.path, mb.CSRF, body); code != http.StatusForbidden || out["error"] != "Only an admin may connect models" {
			t.Fatalf("bob %s %s = %d %v", r.method, r.path, code, out)
		}
	}
	for _, p := range e.home.Paths {
		if !strings.HasPrefix(p, "GET /api/model/options") {
			t.Fatalf("a refused call reached Hermes: %s", p)
		}
	}
	if strings.Contains(e.logs.String(), canary) {
		t.Fatal("the log holds the key")
	}
}
