package web_test

import (
	"context"
	"net/http"
	"testing"
)

// The events stream of a chat Hermes no longer has answers 404 at once and
// leaves the connection and every other request alone.
func TestEventsOfGoneChatIs404(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	login(t, e, c)
	for range 3 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.url+"/api/sessions/pc/local-gone/events", http.NoBody)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("events of a gone chat = %d, want 404", resp.StatusCode)
		}
	}
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions", "", nil, nil); s != http.StatusOK {
		t.Fatalf("sessions after = %d", s)
	}
}
