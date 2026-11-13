package web_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg/agentcfgtest"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

const (
	testBot      = "@test_bot"
	keyApprovals = "approvals_mode"
	keyEnabled   = "enabled"
	keyToken     = "token"
	pSettings    = "/settings"
	pCalendar    = "/connections/home-calendar"
	keyTZ        = "timezone"
	keyURL       = "url"
)

type agentEnv struct {
	*hubEnv
	pc, home *agentcfgtest.Agent
	tg       *fakeTelegram
}

type fakeTelegram struct {
	install bool
	token   string
}

func (f *fakeTelegram) TelegramStatus(context.Context) hub.TelegramStatus {
	return hub.TelegramStatus{Install: f.install, Saved: f.token != "", Running: f.token != "" || f.install, Bot: testBot}
}

func (f *fakeTelegram) TestTelegram(_ context.Context, token string) (string, error) {
	if !strings.Contains(token, ":") {
		return "", &hub.TelegramError{Status: http.StatusBadRequest, Message: "Telegram says this token is not valid."}
	}
	return testBot, nil
}

func (f *fakeTelegram) SaveTelegram(ctx context.Context, token string) (string, error) {
	if f.install {
		return "", &hub.TelegramError{Status: http.StatusConflict, Message: "set by the install"}
	}
	name, err := f.TestTelegram(ctx, token)
	if err == nil {
		f.token = token
	}
	return name, err
}

func (f *fakeTelegram) ClearTelegram(context.Context) error { f.token = ""; return nil }
func (f *fakeTelegram) Steps() string                       { return "collapsed" }

type fixedUpdates struct{ st hub.UpdateStatus }

func (f fixedUpdates) Status(context.Context) hub.UpdateStatus { return f.st }

func setupAgent(t *testing.T, locked ...string) *agentEnv {
	t.Helper()
	pc, home := agentcfgtest.New(), agentcfgtest.New()
	tg := &fakeTelegram{}
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.Agent.LockedKeys = locked
		opts.Agent = agentcfg.New(agentcfg.Options{Access: lateAccess{&e}, Locked: locked,
			Requesters: map[string]device.Requester{"pc": pc, backendHome: home}})
		opts.Telegram = tg
		opts.Snapshot = st.Snapshot
		opts.Scratch = t.TempDir()
		opts.Updates = fixedUpdates{hub.UpdateStatus{Current: "0.26.70", Latest: &hub.Release{Version: "0.27.0"}}}
	})
	return &agentEnv{hubEnv: e, pc: pc, home: home, tg: tg}
}

func TestAgentAdminRoutes(t *testing.T) {
	t.Parallel()
	e := setupAgent(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	base := e.url + "/api/agent/pc"

	code, st, text := call(t, c, http.MethodGet, base+pSettings, "", nil)
	if code != http.StatusOK || st[keyApprovals] != "smart" || st["soul"] != "You are helpful." || st["live"] != true {
		t.Fatalf("settings = %d %s", code, text)
	}
	code, _, text = call(t, c, http.MethodPut, base+pSettings, m.CSRF, map[string]any{keyApprovals: "manual", keyTZ: "Europe/Berlin"})
	if code != http.StatusOK {
		t.Fatalf("save = %d %s", code, text)
	}
	if a, _ := e.pc.Config["approvals"].(map[string]any); a["mode"] != "manual" || e.pc.Config[keyTZ] != "Europe/Berlin" {
		t.Fatalf("agent config = %v", e.pc.Config)
	}
	// Only the keys sent: nothing else reached the agent's config.
	if !strings.Contains(e.pc.Bodies[len(e.pc.Bodies)-1], `"approvals":{"mode":"manual"}`) || strings.Contains(e.pc.Bodies[len(e.pc.Bodies)-1], "compression") {
		t.Fatalf("PUT body = %s", e.pc.Bodies[len(e.pc.Bodies)-1])
	}
	for _, bad := range []map[string]any{{keyApprovals: "yolo"}, {keyTZ: "Mars/Base"}, {"compression_threshold": 0.33}} {
		if code, _, _ := call(t, c, http.MethodPut, base+pSettings, m.CSRF, bad); code != http.StatusBadRequest {
			t.Fatalf("%v = %d", bad, code)
		}
	}
	code, out, _ := call(t, c, http.MethodPut, base+"/soul", m.CSRF, map[string]any{fieldContent: "Answer briefly."})
	if code != http.StatusOK || out["applied_live"] != true || e.pc.Soul != "Answer briefly." {
		t.Fatalf("soul = %d %v", code, out)
	}
	// Reset sends no text and gets Talaria's default, never an empty file (the agent's own text would return).
	code, _, _ = call(t, c, http.MethodPut, base+"/soul", m.CSRF, map[string]any{fieldContent: ""})
	if code != http.StatusOK || e.pc.Soul != agentcfg.DefaultSoul {
		t.Fatalf("reset soul = %d %q", code, e.pc.Soul)
	}
	if _, st, _ = call(t, c, http.MethodGet, base+pSettings, "", nil); st["soul_default"] != agentcfg.DefaultSoul {
		t.Fatalf("settings soul_default = %v", st["soul_default"])
	}
	agentToolsAndConnections(t, e, c, base, m.CSRF)
	agentDiagnostics(t, c, base, m.CSRF)
}

func agentToolsAndConnections(t *testing.T, e *agentEnv, c *http.Client, base, csrf string) {
	t.Helper()
	code, out, _ := call(t, c, http.MethodPut, base+"/tools/web", csrf, map[string]any{keyEnabled: false})
	if code != http.StatusOK || out["applied_live"] != true || e.pc.Toolsets[0][keyEnabled] != false {
		t.Fatalf("toolset = %d %v", code, out)
	}
	if code, _, _ := call(t, c, http.MethodPut, base+"/tools/terminal", csrf, map[string]any{keyEnabled: false}); code != http.StatusConflict {
		t.Fatalf("terminal off = %d, want 409", code)
	}
	code, out, _ = call(t, c, http.MethodPut, base+"/skills/github", csrf, map[string]any{keyEnabled: false})
	if code != http.StatusOK || out["applied_live"] != true {
		t.Fatalf("skill = %d %v", code, out)
	}
	code, out, _ = call(t, c, http.MethodPut, base+pCalendar, csrf, map[string]any{keyEnabled: false})
	if code != http.StatusOK || out["applied_live"] != true || e.pc.Servers[0][keyEnabled] != false {
		t.Fatalf("connection = %d %v", code, out)
	}
	if code, _, _ := call(t, c, http.MethodPost, base+"/connections", csrf, map[string]any{fieldName: "bad name", keyURL: "https://x"}); code != http.StatusBadRequest {
		t.Fatalf("bad name = %d", code)
	}
	if code, _, _ := call(t, c, http.MethodPost, base+"/connections", csrf, map[string]any{fieldName: "x", keyURL: "file:///etc/passwd"}); code != http.StatusBadRequest {
		t.Fatalf("file url = %d", code)
	}
	code, out, _ = call(t, c, http.MethodPost, base+"/connections", csrf, map[string]any{fieldName: "notes", keyURL: "https://notes.example/mcp", keyToken: canary})
	if code != http.StatusOK || len(e.pc.Servers) != 2 {
		t.Fatalf("add = %d %v", code, out)
	}
}

func agentDiagnostics(t *testing.T, c *http.Client, base, csrf string) {
	t.Helper()
	code, _, text := call(t, c, http.MethodGet, base+"/diagnostics", "", nil)
	if code != http.StatusOK || !strings.Contains(text, "Release 0.23.1. Using 312 MB") {
		t.Fatalf("diagnostics = %d %s", code, text)
	}
	code, out, _ := call(t, c, http.MethodPost, base+"/healthcheck", csrf, nil)
	if lines, _ := out["lines"].([]any); code != http.StatusOK || out["ok"] != true || len(lines) != 4 {
		t.Fatalf("health check = %d %v", code, out)
	}
	code, out, _ = call(t, c, http.MethodGet, base+"/log?level=WARNING", "", nil)
	if lines, _ := out["lines"].([]any); code != http.StatusOK || len(lines) != 1 {
		t.Fatalf("log = %d %v", code, out)
	}
	if code, _, _ := call(t, c, http.MethodGet, base+"/log?level=DEBUG", "", nil); code != http.StatusBadRequest {
		t.Fatalf("log level = %d", code)
	}
}

func TestAgentMemberGating(t *testing.T) {
	t.Parallel()
	e := setupAgent(t)
	c := client(t)
	e.is.as("sub-bob", userBob, "family")
	m := login(t, e.hubEnv, c)
	base := e.url + "/api/agent/" + backendHome
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, pSettings}, {http.MethodPut, pSettings}, {http.MethodPut, "/soul"}, {http.MethodGet, "/tools"},
		{http.MethodPut, "/tools/web"}, {http.MethodPut, "/skills/github"}, {http.MethodPut, pCalendar},
		{http.MethodPost, "/connections"}, {http.MethodGet, "/diagnostics"}, {http.MethodGet, "/log"}, {http.MethodPost, "/healthcheck"},
	} {
		if code, _, _ := call(t, c, r.method, base+r.path, m.CSRF, map[string]any{keyEnabled: false}); code != http.StatusForbidden {
			t.Fatalf("member %s %s = %d", r.method, r.path, code)
		}
	}
	for _, p := range []string{"/api/admin/updates", "/api/admin/telegram"} {
		if code, _, _ := call(t, c, http.MethodGet, e.url+p, "", nil); code != http.StatusForbidden {
			t.Fatalf("member %s = %d", p, code)
		}
	}
	if code, _, _ := call(t, c, http.MethodPost, e.url+"/api/admin/backup", m.CSRF, map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("member backup = %d", code)
	}
	if e.home.Seen("PUT ") || e.home.Seen("POST ") {
		t.Fatalf("a member's request reached the agent: %v", e.home.Paths)
	}
	// The quick panel: names and on or off only, no off items, no descriptions.
	code, out, text := call(t, c, http.MethodGet, base+"/summary", "", nil)
	if code != http.StatusOK || out["admin"] != false || out[keyApprovals] != "smart" {
		t.Fatalf("summary = %d %s", code, text)
	}
	if strings.Contains(text, "weather-brief") || strings.Contains(text, "image_gen") || strings.Contains(text, "description") || strings.Contains(text, "calendar.example") {
		t.Fatalf("member summary shows too much: %s", text)
	}
	// A backend the member may not use.
	if code, _, _ := call(t, c, http.MethodGet, e.url+"/api/agent/pc/summary", "", nil); code != http.StatusForbidden {
		t.Fatalf("other backend summary = %d", code)
	}
	var me map[string]any
	_, body := get(t, c, e.url+"/api/me")
	_ = json.Unmarshal([]byte(body), &me)
	if me["agent_admin"] != false || me["telegram_admin"] != false || me["admin"] != false {
		t.Fatalf("member me = %v", me)
	}
}

func TestAgentLockedKeys(t *testing.T) {
	t.Parallel()
	e := setupAgent(t, keyTZ, "soul", "mcp_servers")
	c := client(t)
	m := login(t, e.hubEnv, c)
	base := e.url + "/api/agent/pc"
	_, st, _ := call(t, c, http.MethodGet, base+pSettings, "", nil)
	if locked, _ := st["locked"].([]any); len(locked) != 2 {
		t.Fatalf("locked = %v", st["locked"])
	}
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, pSettings, map[string]any{keyTZ: "UTC"}},
		{http.MethodPut, "/soul", map[string]any{fieldContent: "x"}},
		{http.MethodPut, pCalendar, map[string]any{keyEnabled: false}},
		{http.MethodDelete, pCalendar, nil},
	} {
		code, out, _ := call(t, c, r.method, base+r.path, m.CSRF, r.body)
		if code != http.StatusConflict || out["error"] != "Set in the deployment config, so it cannot be changed here." {
			t.Fatalf("%s %s = %d %v", r.method, r.path, code, out)
		}
	}
	if code, _, _ := call(t, c, http.MethodPut, base+pSettings, m.CSRF, map[string]any{keyApprovals: "off"}); code != http.StatusOK {
		t.Fatalf("an unlocked key = %d", code)
	}
}

func TestAdminUpdatesTelegramBackup(t *testing.T) {
	t.Parallel()
	e := setupAgent(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	code, out, _ := call(t, c, http.MethodGet, e.url+"/api/admin/updates", "", nil)
	if latest, _ := out["latest"].(map[string]any); code != http.StatusOK || latest["version"] != "0.27.0" {
		t.Fatalf("updates = %d %v", code, out)
	}
	if code, out, _ := call(t, c, http.MethodPost, e.url+"/api/admin/telegram/test", m.CSRF, map[string]any{keyToken: "nope"}); code != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("bad token = %d %v", code, out)
	}
	tok := "123456:" + strings.Repeat("A", 30)
	if code, out, _ := call(t, c, http.MethodPut, e.url+"/api/admin/telegram", m.CSRF, map[string]any{keyToken: tok}); code != http.StatusOK || out["bot"] != testBot {
		t.Fatalf("save = %d %v", code, out)
	}
	_, out, text := call(t, c, http.MethodGet, e.url+"/api/admin/telegram", "", nil)
	if out["running"] != true || strings.Contains(text, tok) {
		t.Fatalf("status = %s", text)
	}
	e.tg.install = true
	if code, _, _ := call(t, c, http.MethodPut, e.url+"/api/admin/telegram", m.CSRF, map[string]any{keyToken: tok}); code != http.StatusConflict {
		t.Fatalf("install token = %d", code)
	}

	if err := e.store.SetSetting(context.Background(), "marker", "in-the-backup"); err != nil {
		t.Fatal(err)
	}
	code, out, text = call(t, c, http.MethodPost, e.url+"/api/admin/backup", m.CSRF, map[string]any{fieldBackend: "pc"})
	if code != http.StatusOK || out[keyToken] == "" {
		t.Fatalf("backup = %d %s", code, text)
	}
	tok2, _ := out[keyToken].(string)
	resp, body := get(t, c, e.url+"/api/admin/backup/"+tok2)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("download = %d %s", resp.StatusCode, body)
	}
	z, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		_ = r.Close()
		files[f.Name] = string(b)
	}
	if files["agent/agent-backup.zip"] != "PK-agent-archive" || !strings.Contains(files["README.txt"], "To restore") ||
		!strings.Contains(files["hub/keys.yaml"], "session_secret") || !strings.Contains(files["hub/hub.db"], "in-the-backup") {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		t.Fatalf("backup holds %v", names)
	}
	if r, _ := get(t, c, e.url+"/api/admin/backup/forged"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("forged token = %d", r.StatusCode)
	}
}
