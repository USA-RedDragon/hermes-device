package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const (
	userAlice   = "alice"
	groupAdmins = "admins"
	clientID    = "hub"
)

type hubEnv struct {
	url   string
	is    *issuer
	gw    *gatewaytest.Gateway
	store *store.Store
}

func setup(t *testing.T) *hubEnv {
	t.Helper()
	is := newIssuer(t)
	gw := gatewaytest.New(clientID, "pw")
	t.Cleanup(gw.Close)
	gw.AddSession(&gatewaytest.Session{ID: "s1", Title: "First", Live: true, StartedAt: 100})
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewUnstartedServer(nil)
	pub := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{LogLevel: "info",
		HTTP:    config.HTTP{PublicURL: pub},
		OIDC:    config.OIDC{IssuerURL: is.srv.URL, ClientID: clientID, ClientSecret: "s3cret", Scopes: []string{"openid", "profile"}, UsernameClaim: "preferred_username", GroupsClaim: "groups"},
		Session: config.Session{Secret: strings.Repeat("k", 40), TTL: time.Hour},
		Backends: []config.Backend{
			{ID: "pc", URL: gw.URL(), Username: clientID, Password: "pw", Users: []string{userAlice}},
			{ID: "home", URL: gw.URL(), Username: clientID, Password: "pw", Groups: []string{"family"}},
		}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc := service.New(cfg.Backends, st, service.Options{})
	t.Cleanup(svc.Close)
	s, err := web.New(cfg, web.Options{Service: svc, Links: st, Tokens: st, Groups: st, KeepAlive: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = s.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	return &hubEnv{url: pub, is: is, gw: gw, store: st}
}

func client(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func get(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

type me struct {
	User   string   `json:"user"`
	Groups []string `json:"groups"`
	CSRF   string   `json:"csrf"`
}

func login(t *testing.T, e *hubEnv, c *http.Client) me {
	t.Helper()
	resp, _ := get(t, c, e.url+"/auth/login?next=/")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
		t.Fatalf("login ended at %s with %d", resp.Request.URL, resp.StatusCode)
	}
	resp, body := get(t, c, e.url+"/api/me")
	var m me
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &m) != nil {
		t.Fatalf("me = %d %s", resp.StatusCode, body)
	}
	return m
}

func post(t *testing.T, c *http.Client, u, csrf, origin string, body any) (*http.Response, string) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, u, strings.NewReader(string(b)))
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
	rb, _ := io.ReadAll(resp.Body)
	return resp, string(rb)
}

func TestLoginFlow(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	if resp, _ := get(t, c, e.url+"/api/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous me = %d", resp.StatusCode)
	}
	m := login(t, e, c)
	if m.User != userAlice || len(m.Groups) != 1 || m.Groups[0] != groupAdmins || m.CSRF == "" {
		t.Fatalf("me = %+v", m)
	}
	if g, _ := e.store.Groups(context.Background(), userAlice); len(g) != 1 || g[0] != groupAdmins {
		t.Fatalf("stored groups = %v", g)
	}
	resp, _ := post(t, c, e.url+"/auth/logout", m.CSRF, "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, e.url+"/api/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d", resp.StatusCode)
	}
}

func TestLoginRejectsForgedCallbacks(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	if resp, _ := get(t, c, e.url+"/auth/callback?code=x&state=y"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback without login cookie = %d", resp.StatusCode)
	}
	noFollow := client(t)
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ := get(t, noFollow, e.url+"/auth/login")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("authorize URL = %s", loc)
	}
	if resp, _ := get(t, noFollow, e.url+"/auth/callback?code=x&state=wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback with wrong state = %d", resp.StatusCode)
	}
	e.is.mu.Lock()
	e.is.badNonce = true
	e.is.mu.Unlock()
	resp, _ = get(t, c, e.url+"/auth/login?next=/")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad nonce login = %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, e.url+"/api/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after bad nonce = %d", resp.StatusCode)
	}
}

func TestNextIsSameOrigin(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	for _, next := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example"} {
		resp, _ := get(t, c, e.url+"/auth/login?next="+url.QueryEscape(next))
		if resp.Request.URL.Host != strings.TrimPrefix(e.url, "http://") || resp.Request.URL.Path != "/" {
			t.Fatalf("next %q ended at %s", next, resp.Request.URL)
		}
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	u := e.url + "/api/sessions/pc/s1/interrupt"
	if resp, _ := post(t, c, u, "", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, u, "wrong", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong CSRF = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, u, m.CSRF, "https://evil.example", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross origin = %d", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.url+"/api/sessions/pc/s1/events", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin stream = %d", resp.StatusCode)
	}
	resp, body := post(t, c, e.url+"/api/sessions", m.CSRF, e.url, map[string]string{"backend_id": "pc"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"live":true`) {
		t.Fatalf("new session = %d %s", resp.StatusCode, body)
	}
}

func TestAPIAuthorization(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	e.is.mu.Lock()
	e.is.user, e.is.groups = "bob", []string{"family"}
	e.is.mu.Unlock()
	m := login(t, e, c)
	_, body := get(t, c, e.url+"/api/backends")
	if body != `[{"id":"home","name":"home"}]`+"\n" {
		t.Fatalf("bob backends = %s", body)
	}
	if resp, _ := post(t, c, e.url+"/api/sessions/pc/s1/prompt", m.CSRF, "", map[string]string{"text": "hi"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob prompt on alice's backend = %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, e.url+"/api/sessions/pc/s1/events"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob stream on alice's backend = %d", resp.StatusCode)
	}
}

type sse struct {
	id   string
	kind string
	data json.RawMessage
}

func readEvents(t *testing.T, r *bufio.Reader, until string) []sse {
	t.Helper()
	var out []sse
	var cur sse
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "data: "):
			var d struct {
				Kind string          `json:"kind"`
				Data json.RawMessage `json:"data"`
			}
			_ = json.Unmarshal([]byte(line[6:]), &d)
			cur.kind, cur.data = d.Kind, d.Data
		case line == "" && cur.kind != "":
			out = append(out, cur)
			if cur.kind == until {
				return out
			}
			cur = sse{}
		}
	}
}

func stream(t *testing.T, c *http.Client, u, lastID string) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	sc := *c
	sc.Timeout = 0
	resp, err := sc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return bufio.NewReader(resp.Body), func() { cancel(); _ = resp.Body.Close() }
}

func TestEventStreamAndResume(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	r, stop := stream(t, c, e.url+"/api/sessions/pc/s1/events", "")
	first := readEvents(t, r, "snapshot")
	if len(first) != 1 || !strings.Contains(string(first[0].data), `"title":"First"`) {
		t.Fatalf("first = %+v", first)
	}
	if resp, _ := post(t, c, e.url+"/api/sessions/pc/s1/prompt", m.CSRF, e.url, map[string]string{"text": "hello"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt = %d", resp.StatusCode)
	}
	evs := readEvents(t, r, "message.complete")
	stop()
	kinds := make([]string, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.kind)
	}
	if strings.Join(kinds, ",") != "message.start,reasoning.delta,reasoning.delta,tool.start,tool.progress,tool.complete,message.delta,message.delta,message.complete" {
		t.Fatalf("kinds = %v", kinds)
	}
	r2, stop2 := stream(t, c, e.url+"/api/sessions/pc/s1/events", evs[5].id)
	defer stop2()
	again := readEvents(t, r2, "message.complete")
	if len(again) != 3 || again[0].id != evs[6].id || again[0].kind != "message.delta" {
		t.Fatalf("resumed = %+v", again)
	}
}

func TestTelegramLink(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	tok, err := e.store.CreateLinkToken(ctx, 777)
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	resp, body := get(t, c, e.url+"/link/telegram?token="+url.QueryEscape(tok))
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/link/telegram" || !strings.Contains(body, "Link account") {
		t.Fatalf("link page = %d %s %s", resp.StatusCode, resp.Request.URL, body)
	}
	if _, ok, _ := e.store.UserForTelegram(ctx, 777); ok {
		t.Fatal("GET bound the account")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if csrf == nil {
		t.Fatal("no csrf field")
	}
	form := func(token, csrf string) (*http.Response, string) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.url+"/link/telegram",
			strings.NewReader(url.Values{"token": {token}, "csrf": {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, _ := form(tok, "wrong"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad csrf = %d", resp.StatusCode)
	}
	if resp, body := form(tok, csrf[1]); resp.StatusCode != http.StatusOK || !strings.Contains(body, "now linked") {
		t.Fatalf("link = %d %s", resp.StatusCode, body)
	}
	if u, ok, _ := e.store.UserForTelegram(ctx, 777); !ok || u != userAlice {
		t.Fatalf("linked user = %q %v", u, ok)
	}
	if resp, body := form(tok, csrf[1]); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "used or expired") {
		t.Fatalf("reuse = %d", resp.StatusCode)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	resp, body := get(t, c, e.url+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "/static/app.js") {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers = %v", resp.Header)
	}
	for _, f := range []string{"/static/app.js", "/static/app.css"} {
		if resp, _ := get(t, c, e.url+f); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", f, resp.StatusCode)
		}
	}
}
