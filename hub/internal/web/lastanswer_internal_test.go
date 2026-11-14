package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A slow Hermes does not hold the page: once an answer was read, later
// requests get it at once while the new read runs behind them, and a read
// that hangs does not hold up another route's.
func TestLastAnswerServesWhileHermesIsSlow(t *testing.T) {
	t.Parallel()
	var slow atomic.Bool
	var n atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if slow.Load() {
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		}
		_, _ = w.Write([]byte(`{"n":1}`))
	}
	s := &Server{}
	wrapped := s.lastAnswer(h)
	get := func(path string) (time.Duration, string) {
		req := httptest.NewRequestWithContext(context.WithValue(context.Background(), userKey{}, "alice"), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		start := time.Now()
		wrapped(rec, req)
		return time.Since(start), rec.Body.String()
	}
	if d, b := get("/a"); b != `{"n":1}` || d > time.Second {
		t.Fatalf("first read = %v %q", d, b)
	}
	slow.Store(true)
	time.Sleep(2100 * time.Millisecond) // past the fresh window: a background read starts
	for i := range 3 {
		if d, b := get("/a"); b != `{"n":1}` || d > 200*time.Millisecond {
			t.Fatalf("read %d with a slow Hermes took %v (%q), want it at once", i, d, b)
		}
	}
	if n.Load() > 2 {
		t.Fatalf("Hermes was asked %d times, want one background read at a time", n.Load())
	}
	// A change drops the kept answer: the next read waits for the live one.
	s.last.forget("alice")
	if d, _ := get("/a"); d < 2*time.Second {
		t.Fatalf("read after a change took %v, want it to wait for Hermes", d)
	}
}
