package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

func listed(ctx context.Context, t *testing.T, svc *service.Service, id string) bool {
	t.Helper()
	list, err := svc.Sessions(ctx, userAlice)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

// The chat list is answered from the hub's last list while it is recent,
// read again behind the answer, and read again before the answer when the
// user changed something since (hub.WithListAfter).
func TestChatListFromTheLastList(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := hub.WithCached(context.Background())
	if listed(ctx, t, e.svc, "later") {
		t.Fatal("listed a chat that does not exist yet")
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "later", Title: "Later", Live: true, StartedAt: 500})
	started := time.Now()
	if listed(ctx, t, e.svc, "later") {
		t.Fatal("a recent list was read again from Hermes")
	}
	if d := time.Since(started); d > 50*time.Millisecond {
		t.Fatalf("an answer from the last list took %s", d)
	}
	if !listed(hub.WithListAfter(ctx, time.Now()), t, e.svc, "later") {
		t.Fatal("a list asked for after a change did not show it")
	}
	// A chat started elsewhere shows once the list is read behind.
	e.gw.AddSession(&gatewaytest.Session{ID: "elsewhere", Title: "Elsewhere", Live: true, StartedAt: 600})
	deadline := time.Now().Add(5 * time.Second)
	for !listed(ctx, t, e.svc, "elsewhere") {
		if time.Now().After(deadline) {
			t.Fatal("the list was never read again behind the answer")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
