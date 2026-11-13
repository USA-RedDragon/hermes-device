package service_test

import (
	"context"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

// A viewer that opens a chat the replica has held for a while gets the
// transcript as it is now in one snapshot, not the old snapshot and every
// event of the turns since.
func TestFreshOpenOfWarmChatGetsNewSnapshot(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	first, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	until(t, first, hub.KindSnapshot)
	const turns = 5
	for range turns {
		e.gw.Publish(sLive, hub.KindMessageStart, map[string]any{})
		for range 50 {
			e.gw.Publish(sLive, hub.KindMessageDelta, map[string]any{kText: "words "})
		}
		e.gw.Publish(sLive, hub.KindMessageComplete, map[string]any{kStatus: "complete"})
	}
	for n := 0; n < turns; {
		if next(t, first).Kind == hub.KindMessageComplete {
			n++
		}
	}
	second, stop2, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	evs := until(t, second, hub.KindSnapshot)
	if len(evs) != 1 {
		t.Fatalf("a fresh open began with %v, want one snapshot", kinds(evs))
	}
	e.gw.Publish(sLive, hub.KindMessageStart, map[string]any{})
	e.gw.Publish(sLive, hub.KindMessageDelta, map[string]any{kText: "marker"})
	var after []hub.Event
	for {
		ev := next(t, second)
		after = append(after, ev)
		if ev.Kind == hub.KindMessageDelta && text(t, ev) == "marker" {
			break
		}
	}
	if len(after) != 2 {
		t.Fatalf("%d events after the snapshot before the live one, want 2: the old turns were replayed", len(after))
	}
}
