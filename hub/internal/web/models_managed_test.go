package web_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/models"
	"github.com/USA-RedDragon/talaria/hub/internal/models/modelstest"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

const (
	varDeepSeek = "DEEPSEEK_API_KEY"
	pinnedModel = "claude-sonnet-5-5[1m]"
	pinnedProv  = "claude-pool"
	modelGPT5   = "openai/gpt-5"
)

// text is v as a string, or "".
func text(v any) string {
	s, _ := v.(string)
	return s
}

// managedHermes is a Hermes whose deployment pins the default, sets one
// server and puts a DeepSeek key in the environment.
func managedHermes(readOnly bool) *modelstest.Hermes {
	h := modelstest.New()
	h.Pinned, h.ReadOnly = true, readOnly
	h.Model, h.Provider = pinnedModel, pinnedProv
	h.Endpoints["local-ollama"] = map[string]any{"name": "local-ollama", fieldBaseURL: "http://ollama:11434/v1", keyModel: "qwen3.5:2b"}
	h.Managed["local-ollama"] = true
	h.Keys[varDeepSeek] = ""
	h.InEnvironment[varDeepSeek] = true
	return h
}

func TestModelsSetDefaultReportsAPinnedDefault(t *testing.T) {
	t.Parallel()
	for _, readOnly := range []bool{false, true} {
		e := setupModelsWith(t, managedHermes(readOnly))
		c := client(t)
		m := login(t, e.hubEnv, c)
		base := e.url + "/api/models/pc"
		code, out, body := call(t, c, http.MethodPost, base+"/default", m.CSRF, map[string]any{fieldProv: "deepseek", keyModel: "deepseek-chat"})
		if readOnly {
			if code != http.StatusConflict || !strings.Contains(text(out["error"]), "set in the deployment config") {
				t.Fatalf("read-only Hermes: %d %s", code, body)
			}
		} else if code != http.StatusOK || out["ok"] != false || out["unchanged"] != true || out[keyModel] != pinnedModel {
			t.Fatalf("a kept default must read as unchanged, not success: %d %s", code, body)
		}
		if e.pc.Model != pinnedModel {
			t.Fatalf("default moved to %s", e.pc.Model)
		}
		for _, b := range e.pc.Bodies {
			if strings.Contains(b, "base_url") && strings.Contains(b, `"scope"`) {
				t.Fatalf("set default sent a base_url: %s", b)
			}
		}
		// Making a server the default is checked the same way.
		code, out, body = call(t, c, http.MethodPost, base+"/endpoints/local-ollama/use", m.CSRF, nil)
		if readOnly != (code == http.StatusConflict) || (!readOnly && out["unchanged"] != true) {
			t.Fatalf("use a server with a pinned default (read-only %v): %d %s", readOnly, code, body)
		}
	}
}

func TestModelsSetDefaultChecksTheResult(t *testing.T) {
	t.Parallel()
	e := setupModels(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	base := e.url + "/api/models/pc"
	e.pc.Keys[varOpenRouter] = "sk-or-v1-abcdefabcdef"
	code, out, body := call(t, c, http.MethodPost, base+"/default", m.CSRF, map[string]any{fieldProv: provOpenRouter, keyModel: modelGPT5, fieldBaseURL: "https://evil.example/v1"})
	if code != http.StatusOK || out["ok"] != true || out[keyModel] != modelGPT5 {
		t.Fatalf("set default: %d %s", code, body)
	}
	if strings.Contains(strings.Join(e.pc.Bodies, "\n"), "evil.example") {
		t.Fatal("the hub passed a base_url on to Hermes")
	}
	// The price warning still asks first.
	code, out, body = call(t, c, http.MethodPost, base+"/default", m.CSRF, map[string]any{fieldProv: provOpenRouter, keyModel: "anthropic/claude-opus"})
	if code != http.StatusOK || out["confirm_required"] != true || e.pc.Model != modelGPT5 {
		t.Fatalf("expensive model without confirm: %d %s", code, body)
	}
}

func TestModelsDeploymentItemsAreReadOnly(t *testing.T) {
	t.Parallel()
	e := setupModelsWith(t, managedHermes(true))
	c := client(t)
	m := login(t, e.hubEnv, c)
	base := e.url + "/api/models/pc"
	code, out, body := call(t, c, http.MethodGet, base+"/connected", "", nil)
	if code != http.StatusOK {
		t.Fatalf("connected: %d %s", code, body)
	}
	pinned, _ := out["pinned"].(map[string]any)
	if pinned == nil || pinned[keyModel] != pinnedModel || !strings.Contains(text(pinned["source"]), "/etc/hermes-managed") {
		t.Fatalf("pinned default not reported: %s", body)
	}
	if !strings.Contains(body, `"managed":true`) {
		t.Fatalf("managed server not marked: %s", body)
	}
	keys, _ := out["keys"].([]any)
	var key map[string]any
	if len(keys) == 1 {
		key, _ = keys[0].(map[string]any)
	}
	if key == nil || key["deployment"] != true || key["set"] != true {
		t.Fatalf("the environment's key must show as set in the deployment: %s", body)
	}
	before := len(e.pc.Paths)
	for _, r := range []struct{ method, path string }{{http.MethodPut, "/keys"}, {http.MethodDelete, "/keys/" + varDeepSeek}} {
		code, out, body = call(t, c, r.method, base+r.path, m.CSRF, map[string]string{fieldKey: varDeepSeek, fieldValue: canary})
		if code != http.StatusConflict || !strings.Contains(text(out["error"]), "set in the deployment") {
			t.Fatalf("%s %s: %d %s", r.method, r.path, code, body)
		}
	}
	for _, p := range e.pc.Paths[before:] {
		if strings.HasPrefix(p, "PUT /api/env") || strings.HasPrefix(p, "DELETE /api/env") {
			t.Fatalf("a refused key write reached Hermes: %s", p)
		}
	}
	code, out, body = call(t, c, http.MethodDelete, base+"/endpoints/local-ollama", m.CSRF, nil)
	if code != http.StatusConflict || !strings.Contains(text(out["error"]), "deployment config") {
		t.Fatalf("remove a managed server: %d %s", code, body)
	}
	if _, ok := e.pc.Endpoints["local-ollama"]; !ok {
		t.Fatal("the managed server is gone")
	}
}

func TestModelsHideAnthropicWithClaudeAccounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		pool, plugin bool
		hidden       bool
	}{{"plain", false, false, false}, {"pool provider", true, false, true}, {"accounts plugin", false, true, true}} {
		pc, home := modelstest.New(), modelstest.New()
		pc.ClaudePool = tc.pool
		var e *hubEnv
		e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
			opts.Logger = slog.New(slog.DiscardHandler)
			opts.Models = models.New(models.Options{Access: lateAccess{&e}, Backends: cfg.Backends,
				Requesters: map[string]device.Requester{"pc": pc, backendHome: home},
				Claude:     func(_ context.Context, _, backendID string) bool { return tc.plugin && backendID == "pc" }})
		})
		c := client(t)
		login(t, e, c)
		_, body := get(t, c, e.url+"/api/models/pc/catalog")
		if strings.Contains(body, `"provider":"anthropic"`) == tc.hidden {
			t.Fatalf("%s: anthropic hidden must be %v: %s", tc.name, tc.hidden, body)
		}
	}
}

func TestModelsAdminsPerBackend(t *testing.T) {
	t.Parallel()
	pc, home := modelstest.New(), modelstest.New()
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		cfg.Models.Admins = []string{"bob=home"}
		opts.Models = models.New(models.Options{Access: lateAccess{&e}, Backends: cfg.Backends,
			Requesters: map[string]device.Requester{"pc": pc, backendHome: home}})
	})
	bob := client(t)
	e.is.as("sub-bob", userBob, "family")
	login(t, e, bob)
	if _, body := get(t, bob, e.url+"/api/me"); !strings.Contains(body, `"models_admin":true`) || !strings.Contains(body, `"models_admin_backends":["home"]`) {
		t.Fatalf("bob is the model admin of home: %s", body)
	}
	if code, st, _ := call(t, bob, http.MethodGet, e.url+"/api/models/home/status", "", nil); code != http.StatusOK || st["admin"] != true {
		t.Fatalf("bob's status of home = %d %v", code, st)
	}
	if code, _, body := call(t, bob, http.MethodGet, e.url+"/api/models/home/catalog", "", nil); code != http.StatusOK {
		t.Fatalf("bob reads home's catalog: %d %s", code, body)
	}
	// alice is a hub admin, but models.admins does not name her.
	alice := client(t)
	e.is.as("sub-alice", userAlice)
	login(t, e, alice)
	if _, body := get(t, alice, e.url+"/api/me"); !strings.Contains(body, `"models_admin":false`) {
		t.Fatalf("alice is no model admin once models.admins is set: %s", body)
	}
	if code, out, _ := call(t, alice, http.MethodGet, e.url+"/api/models/pc/catalog", "", nil); code != http.StatusForbidden || out["error"] != "Only an admin may connect models" {
		t.Fatalf("alice reads pc's catalog: %d %v", code, out)
	}
	if pc.Seen("GET /api/env") {
		t.Fatal("a refused call reached Hermes")
	}
}
