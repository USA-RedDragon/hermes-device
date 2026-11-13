package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

func TestOpenDuringRestartWaitsForTheBackend(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	e.gw.BeginRestart()
	e.gw.Stop()
	go func() {
		time.Sleep(700 * time.Millisecond)
		e.gw.FinishRestart()
	}()
	started := time.Now()
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatalf("open during a restart = %v", err)
	}
	defer stop()
	if time.Since(started) < 500*time.Millisecond {
		t.Fatal("open did not wait for the backend")
	}
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", ev.Kind)
	}
}

func TestListDuringRestartKeepsTheLastList(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	before, err := e.svc.Sessions(ctx, userAlice)
	if err != nil || len(before) == 0 {
		t.Fatalf("sessions = %v, %v", before, err)
	}
	e.gw.BeginRestart()
	e.gw.Stop()
	during, err := e.svc.Sessions(ctx, userAlice)
	if err != nil || len(during) != len(before) {
		t.Fatalf("sessions during a restart = %v, %v (before %d)", during, err, len(before))
	}
}
