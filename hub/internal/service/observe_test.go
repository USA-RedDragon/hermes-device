package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

const providerAnthropic = "anthropic"

func TestObservedUsage(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	bucket := map[string]any{"limit": 50, "remaining": 49, "reset_seconds": 12.5, "captured_at": 1.7e9}
	e.gw.Publish(sLive, "session.usage", map[string]any{
		"usage":    map[string]any{"model": "claude", "context_used": 1000, "context_max": 200000, "avg_tps": 42.5, "avg_latency_s": 3.1},
		"provider": providerAnthropic,
		"rate_limit": map[string]any{"requests_min": bucket, "requests_hour": map[string]any{}, "tokens_min": bucket,
			"tokens_hour": map[string]any{}, "captured_at": 1.7e9, "provider": providerAnthropic},
	})
	next(t, ch)
	o, since, err := e.svc.Observed(ctx, userAlice, backendPC)
	if err != nil || since.IsZero() || len(o.Limits) != 1 || len(o.Chats) != 1 {
		t.Fatalf("observed = %+v, %v", o, err)
	}
	l := o.Limits[0]
	if l.Provider != providerAnthropic || l.RequestsMin != (hub.RateBucket{Limit: 50, Remaining: 49, ResetSeconds: 12.5}) || l.At.Unix() != 1.7e9 ||
		l.RequestsHour.Limit != 0 {
		t.Fatalf("limits = %+v", l)
	}
	c := o.Chats[0]
	if c.SessionID != sLive || c.Model != "claude" || c.Provider != providerAnthropic || c.ContextUsed != 1000 || c.ContextMax != 200000 ||
		c.TokensPerSec != 42.5 || c.AvgLatencySecs != 3.1 {
		t.Fatalf("chat = %+v", c)
	}
	e.gw.Publish(sLive, "message.complete", map[string]any{kText: "done", "usage": map[string]any{"context_used": 2000, "context_max": 200000}})
	next(t, ch)
	if o, _, _ := e.svc.Observed(ctx, userAlice, backendPC); o.Chats[0].ContextUsed != 2000 || o.Chats[0].TokensPerSec != 42.5 {
		t.Fatalf("after message.complete = %+v", o.Chats[0])
	}
	if _, _, err := e.svc.Observed(ctx, "mallory", backendPC); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("other user = %v", err)
	}
}
