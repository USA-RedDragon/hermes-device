package web_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

func getWith(t *testing.T, c *http.Client, u string, h map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// The chat list carries an ETag, and a page that has it gets a 304.
func TestSessionsListETag(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	login(t, e, c)
	first := getWith(t, c, e.url+"/api/sessions", nil)
	tag := first.Header.Get("ETag")
	if first.StatusCode != http.StatusOK || tag == "" || strings.Contains(first.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("first = %d etag %q cache %q", first.StatusCode, tag, first.Header.Get("Cache-Control"))
	}
	if again := getWith(t, c, e.url+"/api/sessions", map[string]string{ifNoneMatch: tag}); again.StatusCode != http.StatusNotModified {
		t.Fatalf("same list = %d, want 304", again.StatusCode)
	}
	// A chat started outside the hub shows once the hub reads the list
	// again: the hub answers from its last list and reads it behind.
	e.gw.AddSession(&gatewaytest.Session{ID: "newer", Title: "Newer", Live: true, StartedAt: 500})
	var changed *http.Response
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if changed = getWith(t, c, e.url+"/api/sessions", map[string]string{ifNoneMatch: tag}); changed.StatusCode == http.StatusOK {
			break
		}
	}
	if changed.StatusCode != http.StatusOK || changed.Header.Get("ETag") == tag {
		t.Fatalf("changed list = %d etag %q", changed.StatusCode, changed.Header.Get("ETag"))
	}
}

// The page names each static file by the hash of its bytes, which the
// browser may keep for a year; the same file by any other URL is revalidated.
func TestStaticAssetsAreVersioned(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	login(t, e, c)
	_, page := get(t, c, e.url+"/")
	m := regexp.MustCompile(`src="(/static/app\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("index.html names no versioned app.js")
	}
	if strings.Contains(page, `href="/static/app.css"`) {
		t.Fatal("app.css is not versioned")
	}
	resp := getWith(t, c, e.url+m[1], nil)
	if cc := resp.Header.Get("Cache-Control"); resp.StatusCode != http.StatusOK || !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Fatalf("versioned = %d %q", resp.StatusCode, cc)
	}
	plain := getWith(t, c, e.url+"/static/app.js", nil)
	if cc := plain.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") || plain.Header.Get("ETag") == "" {
		t.Fatalf("unversioned = %q etag %q", cc, plain.Header.Get("ETag"))
	}
	if r := getWith(t, c, e.url+"/static/app.js", map[string]string{ifNoneMatch: plain.Header.Get("ETag")}); r.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidated = %d", r.StatusCode)
	}
	if r := getWith(t, c, e.url+"/static/app.js?v=0000", nil); strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatal("a wrong version is kept for a year")
	}
	idx := getWith(t, c, e.url+"/", nil)
	if r := getWith(t, c, e.url+"/", map[string]string{ifNoneMatch: idx.Header.Get("ETag")}); idx.Header.Get("ETag") == "" || r.StatusCode != http.StatusNotModified {
		t.Fatalf("index revalidation = %d", r.StatusCode)
	}
}

// An open chat's stream tells the page when the chat list changes.
func TestStreamPushesListChanges(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	login(t, e, c)
	r, stop := stream(t, c, e.url+"/api/sessions/pc/s1/events", "")
	defer stop()
	readEvents(t, r, "snapshot")
	time.Sleep(200 * time.Millisecond)
	e.gw.AddSession(&gatewaytest.Session{ID: "fresh", Title: "Fresh", Live: true, StartedAt: 600})
	done := make(chan struct{})
	go func() {
		defer close(done)
		readEvents(t, r, "sessions.changed")
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("no sessions.changed after the list changed")
	}
}

const ifNoneMatch = "If-None-Match"
