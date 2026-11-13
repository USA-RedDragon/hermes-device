package web_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/service"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// A session list the hub could not get from Hermes is never an empty list:
// with the last list it answers that list and names the backend, as
// refreshing while Hermes has been away a short time and as unreachable
// after that; without one it answers 503, so the page keeps what it shows
// and retries.
func TestSessionsWhenHermesIsUnreachable(t *testing.T) {
	t.Parallel()
	e := setupService(t, nil, func(o *service.Options) {
		o.AwayNotice = 300 * time.Millisecond
		o.AwayAlarm = 900 * time.Millisecond
	})
	c := client(t)
	login(t, e, c)
	resp, body := get(t, c, e.url+"/api/sessions")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "First") || resp.Header.Get(web.UnreachableHeader) != "" {
		t.Fatalf("sessions = %d %s", resp.StatusCode, body)
	}
	e.gw.Stop()
	resp, body = get(t, c, e.url+"/api/sessions")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "First") || resp.Header.Get(web.RefreshingHeader) != "pc" ||
		resp.Header.Get(web.UnreachableHeader) != "" {
		t.Fatalf("sessions just after Hermes went down = %d %q %q %s; want the last list, refreshing", resp.StatusCode,
			resp.Header.Get(web.RefreshingHeader), resp.Header.Get(web.UnreachableHeader), body)
	}
	time.Sleep(400 * time.Millisecond)
	resp, body = get(t, c, e.url+"/api/sessions")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "First") || resp.Header.Get(web.UnreachableHeader) != "pc" {
		t.Fatalf("sessions with Hermes down = %d %q %s; want the last list, marked", resp.StatusCode, resp.Header.Get(web.UnreachableHeader), body)
	}

	// Past AwayNotice only: a quiet note, no error. Past AwayAlarm: down.
	if resp.Header.Get(web.DownHeader) != "" {
		t.Fatalf("down header %q after a short silence; want none", resp.Header.Get(web.DownHeader))
	}
	time.Sleep(700 * time.Millisecond)
	resp, _ = get(t, c, e.url+"/api/sessions")
	if resp.Header.Get(web.DownHeader) != "pc" {
		t.Fatalf("down header %q after a long silence; want pc", resp.Header.Get(web.DownHeader))
	}

	fresh := setup(t)
	fresh.gw.Stop()
	c2 := client(t)
	login(t, fresh, c2)
	resp, body = get(t, c2, fresh.url+"/api/sessions")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Cannot reach the agent") || resp.Header.Get(web.UnreachableHeader) != "pc" {
		t.Fatalf("sessions on a fresh hub with Hermes down = %d %s; want 503", resp.StatusCode, body)
	}
}
