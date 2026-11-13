package service

import (
	"context"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const textKey = "text"

type allowAll struct{}

func (allowAll) Groups(context.Context, string) ([]string, error) { return nil, nil }

// Hermes answers a second session.resume on a connection with the
// subscription the connection has, and session.detach with it ends that
// subscription. An idle detach that is still on its way to the gateway
// when a viewer opens the session again must not take the new viewer's
// subscription: the viewer would keep the snapshot it got and see nothing
// that happens after it.
func TestIdleDetachDoesNotEndTheSubscriptionOfANewViewer(t *testing.T) {
	t.Parallel()
	gw := gatewaytest.New("hub", "pw")
	t.Cleanup(gw.Close)
	gw.AddSession(&gatewaytest.Session{ID: "s1", Title: "One", Live: true, StartedAt: 1,
		Messages: []map[string]any{{"role": "user", textKey: "hi", "row_id": 1}}})
	cfg := &config.Config{LogLevel: "info", Users: []config.User{{Sub: "s", Name: "alice"}}, Backends: []config.Backend{
		{ID: "pc", Name: "PC", URL: gw.URL(), Username: "hub", Password: "pw", Users: []string{"alice"}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc := New(cfg.Backends, allowAll{}, Options{IdleDetach: 20 * time.Millisecond})
	t.Cleanup(svc.Close)
	ctx := context.Background()

	opened := make(chan (<-chan hub.Event), 1)
	svc.beforeDetach = func() {
		go func() {
			ch, _, err := svc.Attach(ctx, "alice", "pc", "s1", 0)
			if err != nil {
				t.Error(err)
			}
			opened <- ch
		}()
		// A hub that lets the second viewer open now has it resumed before the
		// detach below reaches the gateway.
		time.Sleep(300 * time.Millisecond)
	}
	ch, stop, err := svc.Attach(ctx, "alice", "pc", "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if e := next(t, ch); e.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", e.Kind)
	}
	stop()

	var ch2 <-chan hub.Event
	select {
	case ch2 = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the second viewer did not open")
	}
	if e := next(t, ch2); e.Kind != hub.KindSnapshot {
		t.Fatalf("second viewer's first event %s", e.Kind)
	}
	// The detach reaches the gateway once the hook returns.
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		seen := false
		for _, c := range gw.Calls() {
			seen = seen || c.Method == gateway.MethodSessionDetach
		}
		if seen {
			break
		}
		if time.Now().After(end) {
			t.Fatal("no session.detach")
		}
	}
	time.Sleep(100 * time.Millisecond)
	gw.Publish("s1", "message.delta", map[string]any{textKey: "later"})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-ch2:
			if e.Kind == "message.delta" {
				return
			}
		case <-deadline:
			t.Fatal("the second viewer got no event after its snapshot: its subscription was ended")
		}
	}
}

func next(t *testing.T, ch <-chan hub.Event) hub.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	return hub.Event{}
}
