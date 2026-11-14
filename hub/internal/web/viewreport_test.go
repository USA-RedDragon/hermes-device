package web_test

import (
	"net/http"
	"strings"
	"testing"
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
