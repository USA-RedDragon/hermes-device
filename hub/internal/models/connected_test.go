package models_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/models"
	"github.com/USA-RedDragon/talaria/hub/internal/models/modelstest"
)

type allowAll struct{}

func (allowAll) Allowed(context.Context, string, string) error { return nil }

// slowHermes answers like the fake Hermes after delay, for every path
// while slow is set.
type slowHermes struct {
	*modelstest.Hermes
	delay time.Duration
	slow  atomic.Bool
	calls atomic.Int32
}

func (h *slowHermes) Request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	h.calls.Add(1)
	if h.slow.Load() {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.Hermes.Request(ctx, method, path, body)
}

func (h *slowHermes) RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return h.Request(ctx, method, path, body)
}

func connectedService(h *slowHermes) *models.Service {
	return models.New(models.Options{Access: allowAll{}, Backends: []config.Backend{{ID: "pc"}},
		Requesters: map[string]device.Requester{"pc": h}})
}

func readConnected(t *testing.T, s *models.Service) time.Duration {
	t.Helper()
	start := time.Now()
	if _, err := s.ModelCall(context.Background(), "alice", "pc", hub.ModelCall{Op: hub.ModelConnected}); err != nil {
		t.Fatalf("connected: %v", err)
	}
	return time.Since(start)
}

// The reads behind the page run together: with every Hermes answer slow,
// the page waits for one of them, not for all four.
func TestConnectedReadsRunTogether(t *testing.T) {
	t.Parallel()
	h := &slowHermes{Hermes: modelstest.New(), delay: 400 * time.Millisecond}
	h.slow.Store(true)
	if d := readConnected(t, connectedService(h)); d > 900*time.Millisecond {
		t.Fatalf("connected took %v with four 400ms reads, want about 400ms", d)
	}
}

// Once the page has been read, it opens from that answer at once, however
// slow Hermes is, and a change made through the hub is not hidden by it.
func TestConnectedServedFromLastAnswer(t *testing.T) {
	t.Parallel()
	h := &slowHermes{Hermes: modelstest.New(), delay: 2 * time.Second}
	s := connectedService(h)
	readConnected(t, s)
	h.slow.Store(true)
	time.Sleep(5100 * time.Millisecond) // past the fresh window: a background read starts
	if d := readConnected(t, s); d > 200*time.Millisecond {
		t.Fatalf("second open took %v with a slow Hermes, want it at once", d)
	}
	if d := readConnected(t, s); d > 200*time.Millisecond {
		t.Fatalf("open during the background read took %v", d)
	}
	// A change through the hub drops the kept answer: the next read waits for Hermes.
	if _, err := s.ModelCall(context.Background(), "alice", "pc", hub.ModelCall{Op: hub.ModelRemoveKey, ID: "NOPE"}); err == nil {
		t.Log("remove accepted")
	}
	if d := readConnected(t, s); d < time.Second {
		t.Fatalf("read after a change took %v, want it to wait for Hermes", d)
	}
}
