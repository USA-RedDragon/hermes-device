package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const (
	keyBackendID = "backend_id"
	titleSecond  = "Second"
	titleThird   = "Third"
	textHello    = "hello"
)

type pageChatJSON struct {
	ID            string `json:"id"`
	BackendID     string `json:"backend_id"`
	Page          bool   `json:"page"`
	BrowserDevice string `json:"browser_device"`
	BrowserError  string `json:"browser_error"`
}

func TestPageChatView(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	resp, body := get(t, c, e.url+"/pagechat")
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<html lang="en" class="pagechat">`) ||
		!strings.Contains(csp, "frame-ancestors 'self' moz-extension:;") || !strings.Contains(csp, "default-src 'self'") ||
		resp.Header.Get("X-Frame-Options") != "" {
		t.Fatalf("page chat view = %d %q %v", resp.StatusCode, csp, resp.Header)
	}
	resp, body = get(t, c, e.url+"/")
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" || strings.Contains(body, `class="pagechat"`) {
		t.Fatalf("hub page headers = %v", resp.Header)
	}
}

func TestPageChatPrompt(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	var pc pageChatJSON
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/pagechats", m.CSRF, map[string]string{keyBackendID: "pc"}, &pc); s != http.StatusCreated ||
		!pc.Page || pc.ID == "" || pc.BrowserDevice != "" {
		t.Fatalf("new page chat = %d %+v", s, pc)
	}
	var list []pageChatJSON
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions", "", nil, &list); s != http.StatusOK {
		t.Fatalf("sessions = %d", s)
	}
	for _, x := range list {
		if (x.ID == pc.ID) != x.Page {
			t.Errorf("session %s page = %v", x.ID, x.Page)
		}
	}
	page := hub.PageContext{Scope: "tab", Tabs: []hub.PageTab{{ID: 7, Title: "Docs", URL: "https://example.com/", Active: true}}}
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/prompt", m.CSRF, map[string]any{keyText: "sum it up", "page": page}, nil); s != http.StatusAccepted {
		t.Fatalf("prompt with page = %d", s)
	}
	row, _ := e.gw.Session("s1")
	var text string
	for _, msg := range row.Messages {
		if msg[kRole] == roleUser {
			text, _ = msg[keyText].(string)
		}
	}
	if !strings.HasPrefix(text, "sum it up\n\n"+hub.PageContextHead) || !strings.Contains(text, "- tab_id 7, active: Docs <https://example.com/>") {
		t.Fatalf("prompt text = %q", text)
	}
	var bad map[string]any
	page.Scope = "everything"
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/prompt", m.CSRF, map[string]any{keyText: "q", "page": page}, &bad); s != http.StatusBadRequest ||
		!strings.Contains(errText(bad), "page context") {
		t.Fatalf("bad page context = %d %v", s, bad)
	}
}

func TestPageChatOnComputer(t *testing.T) {
	t.Parallel()
	e := setupComputers(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	var pc pageChatJSON
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/pagechats", m.CSRF, map[string]string{keyDeviceID: e.id}, &pc); s != http.StatusCreated ||
		pc.BackendID != "pc" || pc.BrowserDevice != e.id || pc.BrowserError != "" {
		t.Fatalf("page chat on a computer = %d %+v", s, pc)
	}
	if got := e.plugin.Browsers()[pc.ID]; got != e.id {
		t.Fatalf("browser device of the page chat = %q", got)
	}
	if _, ok := e.plugin.Bindings()[pc.ID]; ok {
		t.Fatal("a page chat was bound to the computer; it runs in the sandbox")
	}
	e.plugin.Disconnect(e.id)
	var off pageChatJSON
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/pagechats", m.CSRF, map[string]string{keyDeviceID: e.id}, &off); s != http.StatusCreated ||
		off.BrowserDevice != "" || !strings.Contains(off.BrowserError, "not connected") {
		t.Fatalf("page chat with the computer offline = %d %+v", s, off)
	}
	var none map[string]any
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/pagechats", m.CSRF, map[string]string{keyDeviceID: "d_none"}, &none); s != http.StatusNotFound {
		t.Fatalf("page chat on an unknown computer = %d %v", s, none)
	}
}
