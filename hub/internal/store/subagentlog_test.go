package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

func TestSubagentLog(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.UnixMilli(1_800_000_000_000)
		s.SetClock(func() time.Time { return now })
		k := hub.SubagentKey{User: userAlice, BackendID: "b1", SessionID: "s1", SubagentID: "sa-0-a"}
		other := k
		other.User = userBob
		if c, epoch, err := s.SubagentCursor(ctx, k); err != nil || c != "" || epoch != 0 {
			t.Fatalf("new cursor = %q %d %v", c, epoch, err)
		}
		d := int64(2500)
		evs := []hub.SubagentEvent{
			{TS: now, Type: hub.StepGoal, Text: "Read the store"},
			{TS: now.Add(time.Second), Type: hub.StepToolStart, CallID: "c1", Name: "terminal", Args: json.RawMessage(`{"command":"ls"}`)},
			{TS: now.Add(3 * time.Second), Type: hub.StepToolEnd, CallID: "c1", Name: "terminal", Result: "out", ResultSize: 9000,
				DurationMS: &d, Error: true},
		}
		if ok, err := s.AppendSubagentEvents(ctx, k, "", "0:3", false, evs); !ok || err != nil {
			t.Fatalf("append = %v %v", ok, err)
		}
		// A second reader with the old cursor stores nothing.
		if ok, err := s.AppendSubagentEvents(ctx, k, "", "0:3", false, evs); ok || err != nil {
			t.Fatalf("stale append = %v %v", ok, err)
		}
		steer, err := s.AddSubagentSteer(ctx, k, "skip the build", now.Add(4*time.Second))
		if err != nil || steer.Seq != 4 || steer.Type != hub.StepSteer {
			t.Fatalf("steer = %+v %v", steer, err)
		}
		got, err := s.SubagentEvents(ctx, k, 0, 100)
		if err != nil || len(got) != 4 {
			t.Fatalf("events = %+v %v", got, err)
		}
		end := got[2]
		if end.Seq != 3 || end.DurationMS == nil || *end.DurationMS != 2500 || !end.Error || end.ResultSize != 9000 ||
			!end.TS.Equal(now.Add(3*time.Second)) || string(got[1].Args) != `{"command":"ls"}` || got[0].DurationMS != nil {
			t.Fatalf("events = %+v", got)
		}
		if got, _ := s.SubagentEvents(ctx, k, 2, 1); len(got) != 1 || got[0].Seq != 3 {
			t.Fatalf("after 2 limit 1 = %+v", got)
		}
		if got, _ := s.SubagentEvents(ctx, other, 0, 100); len(got) != 0 {
			t.Fatalf("another user's log = %+v", got)
		}
		resetAndPrune(t, s, k, now)
	})
}

// resetAndPrune checks that a reset drops the transcript's events, keeps the
// steer and starts a new epoch, and that old logs are pruned.
func resetAndPrune(t *testing.T, s *store.Store, k hub.SubagentKey, now time.Time) {
	t.Helper()
	ctx := context.Background()
	// A reset drops the transcript's events, keeps the steer, and starts a new epoch.
	if ok, err := s.AppendSubagentEvents(ctx, k, "0:3", "1:1", true, []hub.SubagentEvent{{TS: now, Type: hub.StepGoal, Text: "again"}}); !ok || err != nil {
		t.Fatalf("reset = %v %v", ok, err)
	}
	got, _ := s.SubagentEvents(ctx, k, 0, 100)
	if c, epoch, _ := s.SubagentCursor(ctx, k); len(got) != 2 || got[0].Type != hub.StepSteer || got[1].Seq != 5 || c != "1:1" || epoch != 1 {
		t.Fatalf("after reset = %+v cursor %q epoch %d", got, c, epoch)
	}
	// Logs nobody wrote for long are pruned.
	if err := s.PruneSubagentLogs(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SubagentEvents(ctx, k, 0, 100); len(got) != 0 {
		t.Fatalf("after prune = %+v", got)
	}
}

// A step with no time is stored without one and read back without one.
func TestSubagentLogKeepsUnknownTimeUnset(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		k := hub.SubagentKey{User: userAlice, BackendID: "b1", SessionID: "s1", SubagentID: "sa-0-z"}
		evs := []hub.SubagentEvent{{Type: hub.StepGoal, Text: "go"}, {TS: time.UnixMilli(1_800_000_000_000), Type: hub.StepMessage, Text: "hi"}}
		if ok, err := s.AppendSubagentEvents(ctx, k, "", "0:2", false, evs); !ok || err != nil {
			t.Fatalf("append = %v %v", ok, err)
		}
		got, err := s.SubagentEvents(ctx, k, 0, 10)
		if err != nil || len(got) != 2 || !got[0].TS.IsZero() || got[1].TS.UnixMilli() != 1_800_000_000_000 {
			t.Fatalf("events = %+v %v", got, err)
		}
	})
}
