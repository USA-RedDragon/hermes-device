package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const (
	fileHTML = "a.html"
	noStore  = "no-store"
	script   = "<script>alert(1)</script>"
)

// fakeInsights answers as one backend "pc" that alice may use.
type fakeInsights struct{}

func (fakeInsights) Search(_ context.Context, user, q string) (hub.SearchReport, error) {
	if q == "" {
		return hub.SearchReport{}, fmt.Errorf("%w: a search is 1 to 200 characters", hub.ErrBadQuery)
	}
	return hub.SearchReport{Results: []hub.SearchResult{{BackendID: "pc", SessionID: "s1", Title: user + " <b>",
		Hits: []hub.SearchHit{{RowID: 3, Snippet: ">>>" + q + "<<<"}}}}}, nil
}

func (fakeInsights) Usage(_ context.Context, user, backendID string, days int) (hub.UsageReport, error) {
	if backendID != "pc" || user != userAlice {
		return hub.UsageReport{}, hub.ErrForbidden
	}
	return hub.UsageReport{BackendID: backendID, Days: days}, nil
}

// fakeFiles shows alice's chat s1 and refuses everything else.
type fakeFiles struct{}

func (fakeFiles) check(user, sid string) error {
	if user != userAlice {
		return hub.ErrForbidden
	}
	if sid != "s1" {
		return hub.ErrNoFolder
	}
	return nil
}

func (f fakeFiles) ChatFolder(_ context.Context, user, _, sid, p string) (hub.ChatFolder, error) {
	if err := f.check(user, sid); err != nil {
		return hub.ChatFolder{}, err
	}
	if p == "/etc" {
		return hub.ChatFolder{}, fmt.Errorf("%w: /etc is outside the device roots", hub.ErrFileRefused)
	}
	return hub.ChatFolder{Path: "/src", Dir: "/src", Entries: []hub.FolderEntry{{Name: fileHTML, Kind: "file", Size: 30}}}, nil
}

func (f fakeFiles) ChatFile(_ context.Context, user, _, sid, _ string) (*hub.MediaFile, error) {
	if err := f.check(user, sid); err != nil {
		return nil, err
	}
	return &hub.MediaFile{Name: fileHTML, Size: int64(len(script)), Body: io.NopCloser(strings.NewReader(script))}, nil
}

func (f fakeFiles) ChatDiff(_ context.Context, user, _, sid string) (hub.ChatDiff, error) {
	return hub.ChatDiff{Git: true, Diff: "+x\n"}, f.check(user, sid)
}

func (f fakeFiles) ChatChanges(_ context.Context, user, _, sid string) (hub.ChatChanges, error) {
	return hub.ChatChanges{Files: []hub.WrittenFile{{Path: fileHTML}}}, f.check(user, sid)
}

func TestInsightsAPI(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(_ *config.Config, opts *web.Options, _ *store.Store) {
		opts.Insights, opts.Files = fakeInsights{}, fakeFiles{}
	})
	c := client(t)
	if resp, _ := get(t, c, e.url+"/api/search?q=x"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("search without login = %d", resp.StatusCode)
	}
	login(t, e, c)
	_, body := get(t, c, e.url+"/api/me")
	if !strings.Contains(body, `"usage":true`) || !strings.Contains(body, `"files":true`) {
		t.Fatalf("me = %s", body)
	}
	resp, body := get(t, c, e.url+"/api/search?q=lisbon")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"\u003e\u003e\u003elisbon\u003c\u003c\u003c"`) || !strings.Contains(body, `"row_id":3`) ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("search = %d %s", resp.StatusCode, body)
	}
	if resp, body := get(t, c, e.url+"/api/search?q="); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "1 to 200") {
		t.Fatalf("empty search = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, c, e.url+"/api/usage/pc?days=7"); resp.StatusCode != http.StatusOK {
		t.Fatalf("usage = %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, e.url+"/api/usage/home"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("usage of another backend = %d", resp.StatusCode)
	}
	base := e.url + "/api/sessions/pc/s1"
	for _, p := range []string{"/folder", "/diff", "/changes"} {
		if resp, body := get(t, c, base+p); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", p, resp.StatusCode, body)
		}
	}
	resp, body = get(t, c, base+"/file?path=/src/a.html")
	h := resp.Header
	if resp.StatusCode != http.StatusOK || body != script || h.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("Content-Security-Policy") != "default-src 'none'; sandbox" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != noStore {
		t.Fatalf("file = %d %v", resp.StatusCode, h)
	}
	if resp, body := get(t, c, base+"/folder?path=/etc"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "outside the device roots") {
		t.Fatalf("refused folder = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, c, e.url+"/api/sessions/pc/s2/folder"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat without a folder = %d", resp.StatusCode)
	}
}
