package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const (
	pageHTML  = "<!doctype html>\n<html><head><title>Pick &amp; send</title></head><body><script>hermes.send('x')</script></body></html>"
	pageFile  = "/srv/out/pages/pick.html"
	pageQuirk = "<p>no doctype</p>"
)

func pageURL(e *hubEnv, backend, q string) string {
	return e.url + "/api/sessions/" + backend + "/m1/page?" + q
}

func getAs(t *testing.T, c *http.Client, u, dest string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if dest != "" {
		req.Header.Set("Sec-Fetch-Dest", dest)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestPages(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(cfg *config.Config, _ *web.Options, _ *store.Store) {
		cfg.Backends[0].MediaRoots = []string{"/srv/out"}
	})
	e.gw.SetFile(pageFile, []byte("\xef\xbb\xbf<!-- made by hermes -->\n<!DOCTYPE html><title>File page</title><p>file</p>"))
	e.gw.SetFile("/srv/out/pages/unnamed.html", []byte("<p>x</p>"))
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		{kRole: roleUser, kText: "```page\n" + pageQuirk + "\n```"},
		{kRole: roleAssistant, kText: "Here:\n```page\n" + pageHTML + "\n```\nAnd MEDIA:" + pageFile},
	}})
	id, userID := hub.PageID(pageHTML), hub.PageID(pageQuirk)
	c := client(t)
	login(t, e, c)

	resp, body := getAs(t, c, pageURL(e, "pc", "id="+id), "iframe")
	h := resp.Header
	csp := h.Get("Content-Security-Policy")
	if resp.StatusCode != http.StatusOK || h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("X-Frame-Options") != "" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("page = %d %v", resp.StatusCode, h)
	}
	for _, want := range []string{"sandbox allow-scripts allow-forms allow-modals allow-popups;", "default-src 'none'", "connect-src 'none'",
		"form-action 'none'", "frame-src 'none'", "frame-ancestors " + e.url} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "'self'") || strings.Contains(csp, "allow-top-navigation") {
		t.Errorf("CSP %q lets the page reach the hub", csp)
	}
	if !strings.HasPrefix(body, "<!doctype html><script>(function(){") || !strings.HasSuffix(body, "</script>\n"+pageHTML[len("<!doctype html>\n"):]) {
		t.Fatalf("page body = %q", body)
	}

	resp, body = getAs(t, c, pageURL(e, "pc", "path="+url.QueryEscape(pageFile)), "iframe")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "<!-- made by hermes -->\n<!DOCTYPE html><script>") ||
		!strings.HasSuffix(body, "</script><title>File page</title><p>file</p>") {
		t.Fatalf("file page = %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, c, e.url+"/api/sessions/pc/m1/page-info?path="+url.QueryEscape(pageFile))
	if resp.StatusCode != http.StatusOK || body != "{\"title\":\"File page\"}\n" {
		t.Fatalf("page info = %d %q", resp.StatusCode, body)
	}

	resp, _ = getAs(t, noRedirect(c), pageURL(e, "pc", "id="+id), "document")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#/page/pc/m1/"+id {
		t.Fatalf("top-level page = %d %v", resp.StatusCode, resp.Header)
	}

	for _, q := range []string{"id=" + userID, "id=" + strings.Repeat("0", 32), "id=../x", "id=" + id + "&path=" + url.QueryEscape(pageFile),
		"path=" + url.QueryEscape("/srv/out/pages/unnamed.html"), "path=" + url.QueryEscape("/srv/out/report.pdf"), ""} {
		if resp, body = getAs(t, c, pageURL(e, "pc", q), "iframe"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%q = %d %s", q, resp.StatusCode, body)
		}
	}
	if resp, _ = getAs(t, c, pageURL(e, backendHome, "id="+id), "iframe"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("other user's backend = %d", resp.StatusCode)
	}
	if resp, _ = getAs(t, client(t), pageURL(e, "pc", "id="+id), "iframe"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", resp.StatusCode)
	}
	e.is.as("sub-bob", userBob, "family")
	bob := client(t)
	login(t, e, bob)
	if resp, _ = getAs(t, bob, pageURL(e, "pc", "id="+id), "iframe"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bob = %d", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, pageURL(e, "pc", "id="+id), nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	cross, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = cross.Body.Close()
	if cross.StatusCode != http.StatusForbidden {
		t.Errorf("a request from the page's own opaque origin = %d", cross.StatusCode)
	}
}

func TestPageFromLiveReply(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200})
	c := client(t)
	login(t, e, c)
	id := hub.PageID(pageQuirk)
	if resp, _ := getAs(t, c, pageURL(e, "pc", "id="+id), "iframe"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before the reply = %d", resp.StatusCode)
	}
	e.gw.Publish("m1", "message.start", map[string]any{})
	e.gw.Publish("m1", "message.delta", map[string]any{kText: "Here.\n```page\n<p>no"})
	e.gw.Publish("m1", "message.delta", map[string]any{kText: " doctype</p>\n```"})
	e.gw.Publish("m1", "message.complete", map[string]any{"status": "complete"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, body := getAs(t, c, pageURL(e, "pc", "id="+id), "iframe")
		if resp.StatusCode == http.StatusOK {
			if !strings.HasPrefix(body, "<script>") || !strings.HasSuffix(body, "</script>"+pageQuirk) {
				t.Fatalf("body = %q", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the reply = %d %s", resp.StatusCode, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func noRedirect(c *http.Client) *http.Client {
	n := *c
	n.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &n
}
