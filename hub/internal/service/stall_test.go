package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

// A new hub replica whose gateway connection stopped answering (Hermes
// stuck on one request of that connection) lists again on a new
// connection instead of serving an empty chat list.
func TestListAfterAStallUsesANewConnection(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{StallAfter: 200 * time.Millisecond, CallTimeout: 300 * time.Millisecond})
	release := e.gw.StallNext(gateway.MethodSessionList)
	t.Cleanup(release)
	ss, err := e.svc.Sessions(context.Background(), userAlice)
	if err != nil || len(ss) == 0 {
		t.Fatalf("sessions = %v, %v; want the chats from a new connection", ss, err)
	}
}

// A list that fails is never an empty list: with no list to stand in, the
// error names the backend.
func TestFailedListIsNotEmpty(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	e.gw.Stop()
	ss, err := e.svc.Sessions(context.Background(), userAlice)
	ue := hub.Unreachable(err)
	if ue == nil || ue.Backends[backendPC] == "" || ue.Stale[backendPC] || len(ss) != 0 {
		t.Fatalf("sessions = %v, %v; want an UnreachableError for %s", ss, err, backendPC)
	}
	status, _ := e.svc.BackendStatus(context.Background(), userAlice)
	if status[backendPC] == "" {
		t.Fatal("backend status does not say the backend is unreachable")
	}
}

// With a recent list the hub serves it, marked stale.
func TestFailedListServesTheLastOneMarkedStale(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	before, err := e.svc.Sessions(context.Background(), userAlice)
	if err != nil {
		t.Fatal(err)
	}
	e.gw.Stop()
	during, err := e.svc.Sessions(context.Background(), userAlice)
	ue := hub.Unreachable(err)
	if ue == nil || !ue.Stale[backendPC] || len(during) != len(before) {
		t.Fatalf("sessions = %d rows, %v; want the %d rows from before, marked stale", len(during), err, len(before))
	}
}

// A send whose prompt.submit stalls the connection is held and sent on a
// new connection, not failed.
func TestStalledSendIsHeldAndSent(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{StallAfter: 200 * time.Millisecond, CallTimeout: 300 * time.Millisecond,
		HoldRetryMin: 20 * time.Millisecond, HoldRetryMax: 50 * time.Millisecond})
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	release := e.gw.StallNext(gateway.MethodPromptSubmit)
	t.Cleanup(release)
	res, err := e.svc.Prompt(context.Background(), userAlice, backendPC, sLive, "hello")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v, %v; want it held", res, err)
	}
	until(t, ch, hub.KindInputSent)
}
