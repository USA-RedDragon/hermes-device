package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// A session list the hub could not get from Hermes is never an empty list:
// with the last list it answers that list and names the backend; without
// one it answers 503, so the page keeps what it shows and retries.
func TestSessionsWhenHermesIsUnreachable(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	login(t, e, c)
	resp, body := get(t, c, e.url+"/api/sessions")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "First") || resp.Header.Get(web.UnreachableHeader) != "" {
		t.Fatalf("sessions = %d %s", resp.StatusCode, body)
	}
	e.gw.Stop()
	resp, body = get(t, c, e.url+"/api/sessions")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "First") || resp.Header.Get(web.UnreachableHeader) != "pc" {
		t.Fatalf("sessions with Hermes down = %d %q %s; want the last list, marked", resp.StatusCode, resp.Header.Get(web.UnreachableHeader), body)
	}

	fresh := setup(t)
	fresh.gw.Stop()
	c2 := client(t)
	login(t, fresh, c2)
	resp, body = get(t, c2, fresh.url+"/api/sessions")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Cannot reach Hermes") || resp.Header.Get(web.UnreachableHeader) != "pc" {
		t.Fatalf("sessions on a fresh hub with Hermes down = %d %s; want 503", resp.StatusCode, body)
	}
}
