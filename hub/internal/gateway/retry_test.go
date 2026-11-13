package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/coder/websocket"
)

// heldOnce answers its first call only after hold, the way a stopping Hermes
// gateway holds a call until the next one took over, and every later call at once.
type heldOnce struct {
	calls atomic.Int32
	hold  time.Duration
	next  http.HandlerFunc
}

func (h *heldOnce) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.calls.Add(1) == 1 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(h.hold):
		}
	}
	h.next(w, r)
}

func okJSON(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func TestHeldTicketAndLoginAreTriedAgain(t *testing.T) {
	t.Parallel()
	login := &heldOnce{hold: time.Second, next: okJSON(map[string]bool{"ok": true})}
	ticket := &heldOnce{hold: time.Second, next: okJSON(map[string]string{"ticket": "t1"})}
	mux := http.NewServeMux()
	mux.Handle("POST /auth/password-login", login)
	mux.Handle("POST /api/webapp/socket-ticket", ticket)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := gateway.NewDialerWithAttempt(gateway.Credentials{BaseURL: srv.URL}, 200*time.Millisecond)
	ctx := context.Background()
	if err := d.LoginForTest(ctx); err != nil {
		t.Fatalf("login = %v", err)
	}
	got, err := d.TicketForTest(ctx)
	if err != nil || got != "t1" {
		t.Fatalf("ticket = %q, %v", got, err)
	}
	if login.calls.Load() != 2 || ticket.calls.Load() != 2 {
		t.Fatalf("calls: login %d, ticket %d; want 2 each", login.calls.Load(), ticket.calls.Load())
	}
}

func TestHeldRequestIsTriedAgainOnlyWhenIdempotent(t *testing.T) {
	t.Parallel()
	read := &heldOnce{hold: time.Second, next: okJSON(map[string]int{"jobs": 0})}
	write := &heldOnce{hold: time.Second, next: okJSON(map[string]bool{"ok": true})}
	post := &heldOnce{hold: time.Second, next: okJSON(map[string]bool{"ok": true})}
	mux := http.NewServeMux()
	mux.Handle("GET /api/cron/jobs", read)
	mux.Handle("PUT /api/config", write)
	mux.Handle("POST /api/plugins/x", post)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := gateway.NewDialerWithAttempt(gateway.Credentials{BaseURL: srv.URL}, 200*time.Millisecond)
	ctx := context.Background()
	for _, tc := range []struct{ method, path string }{{http.MethodGet, "/api/cron/jobs"}, {http.MethodPut, "/api/config"}} {
		resp, err := d.Request(ctx, tc.method, tc.path, []byte(`{}`))
		if err != nil {
			t.Fatalf("%s %s = %v", tc.method, tc.path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if _, err := d.Request(ctx, http.MethodPost, "/api/plugins/x", []byte(`{}`)); !errors.Is(err, gateway.ErrUnreachable) {
		t.Fatalf("held POST = %v, want the timeout (it may have run)", err)
	}
	if read.calls.Load() != 2 || write.calls.Load() != 2 || post.calls.Load() != 1 {
		t.Fatalf("calls: GET %d, PUT %d, POST %d; want 2, 2, 1", read.calls.Load(), write.calls.Load(), post.calls.Load())
	}
}

func TestRetryStopsWithTheCaller(t *testing.T) {
	t.Parallel()
	// Every call is held.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	d := gateway.NewDialerWithAttempt(gateway.Credentials{BaseURL: srv.URL}, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.Request(ctx, http.MethodGet, "/x", nil); err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v; want about the caller's 600ms", d)
	}
}

func TestDialMintsAgainWhenTheTicketIsRefused(t *testing.T) {
	t.Parallel()
	var tickets, dials atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webapp/socket-ticket", func(w http.ResponseWriter, _ *http.Request) {
		tickets.Add(1)
		okJSON(map[string]string{"ticket": "t"})(w, nil)
	})
	mux.HandleFunc("GET /api/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{gateway.Subprotocol}})
		if err != nil {
			return
		}
		if dials.Add(1) == 1 {
			// What a stopping gateway sends when the gateway it forwarded the dial to refused the ticket.
			_ = c.Close(websocket.StatusPolicyViolation, "")
			return
		}
		ready := `{"jsonrpc":"2.0","method":"event","params":{"type":"` + gateway.ReadyEvent +
			`","payload":{"session_authority":true}}}`
		_ = c.Write(r.Context(), websocket.MessageText, []byte(ready))
		_, _, _ = c.Read(r.Context())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := gateway.NewDialerWithAttempt(gateway.Credentials{BaseURL: srv.URL}, time.Second)
	c, err := d.Dial(context.Background(), nil)
	if err != nil {
		t.Fatalf("dial = %v", err)
	}
	c.Close()
	if tickets.Load() != 2 || dials.Load() != 2 {
		t.Fatalf("tickets %d, dials %d; want 2 each", tickets.Load(), dials.Load())
	}
}
