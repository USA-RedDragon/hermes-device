package web_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// A page reports a chat that left its newest message after an open; the
// hub takes a bounded JSON report from a signed-in page only.
func TestViewReport(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	report := map[string]any{fieldBackend: "pc", "chat": "s1", "gap": 900, "trail": []any{map[string]any{"what": "scroll"}}}
	if resp, _ := post(t, c, e.url+"/api/view-report", m.CSRF, "", report); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/view-report", "", "", report); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("report without CSRF = %d", resp.StatusCode)
	}
	big := map[string]any{"trail": strings.Repeat("x", 20<<10)}
	if resp, _ := post(t, c, e.url+"/api/view-report", m.CSRF, "", big); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized report = %d", resp.StatusCode)
	}
	if resp, _ := post(t, client(t), e.url+"/api/view-report", m.CSRF, "", report); resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("report from no session = %d", resp.StatusCode)
	}
}

// A beacon cannot set the CSRF header: its token in the body is taken and
// never logged. A page load report is logged as such, with its release.
func TestViewReportBeaconAndLoaded(t *testing.T) {
	t.Parallel()
	sink := &lockedBuf{}
	e := setupWith(t, func(_ *config.Config, o *web.Options, _ *store.Store) {
		o.Logger = slog.New(slog.NewJSONHandler(sink, nil))
	})
	c := client(t)
	m := login(t, e, c)
	if resp, _ := post(t, c, e.url+"/api/view-report", "", "", map[string]any{kKind: "left", "chat": "abcd1234", "csrf": m.CSRF}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("beacon with the token in the body = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/view-report", "", "", map[string]any{kKind: "left", "csrf": "wrong"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("beacon with a wrong token = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/view-report", m.CSRF, "", map[string]any{kKind: "loaded", "version": "v9"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("loaded = %d", resp.StatusCode)
	}
	var left, loaded int
	for _, l := range sink.lines() {
		rep, _ := l["report"].(string)
		if strings.Contains(rep, m.CSRF) || strings.Contains(rep, "csrf") {
			t.Errorf("the log holds the token: %s", rep)
		}
		switch l["msg"] {
		case "view left the newest message":
			left++
		case "page loaded":
			loaded++
		}
	}
	if left != 1 || loaded != 1 {
		t.Fatalf("logged %d left and %d loaded lines, want 1 and 1", left, loaded)
	}
}

const kKind = "kind"
