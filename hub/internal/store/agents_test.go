package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

func TestAgentMessages(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.UnixMilli(1_800_000_000_000)
		s.SetClock(func() time.Time { return now })
		add := func(id, root, to string, hop int, at time.Time) {
			t.Helper()
			if err := s.AddAgentMessage(ctx, hub.AgentMessage{ID: id, BackendID: "b1", FromSession: root, FromRoot: root,
				ToSession: to, Hop: hop, Text: "hi " + id, Mode: hub.AgentModeInbox, Status: hub.AgentStatusSending,
				CreatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		add("am_old", "s1", "s2", 1, now.Add(-40*24*time.Hour))
		add("am_1", "s1", "s2", 1, now.Add(-20*time.Minute))
		add("am_2", "s2", "s1", 2, now.Add(-5*time.Minute))
		add("am_3", "s1", "s3", 1, now)
		if _, ok, _ := s.AgentMessage(ctx, "b1", "am_old"); ok {
			t.Fatal("an old message was kept")
		}
		if _, ok, _ := s.AgentMessage(ctx, "b2", "am_1"); ok {
			t.Fatal("a message of another backend was found")
		}
		if err := s.SetAgentMessageStatus(ctx, "am_1", hub.AgentModeInput, hub.AgentStatusDelivered); err != nil {
			t.Fatal(err)
		}
		m, ok, err := s.AgentMessage(ctx, "b1", "am_1")
		if err != nil || !ok || m.Mode != hub.AgentModeInput || m.Status != hub.AgentStatusDelivered || m.Text != "hi am_1" ||
			!m.CreatedAt.Equal(now.Add(-20*time.Minute)) {
			t.Fatalf("message = %+v %v %v", m, ok, err)
		}
		if hop, err := s.MaxAgentHop(ctx, "b1", []string{"s1", "s9"}, now.Add(-30*time.Minute)); err != nil || hop != 2 {
			t.Fatalf("hop = %d %v", hop, err)
		}
		if hop, err := s.MaxAgentHop(ctx, "b1", []string{"s2", "s3"}, now.Add(-30*time.Minute)); err != nil || hop != 1 {
			t.Fatalf("hop = %d %v", hop, err)
		}
		if hop, err := s.MaxAgentHop(ctx, "b1", []string{"s1"}, now.Add(-time.Minute)); err != nil || hop != 0 {
			t.Fatalf("hop outside the window = %d %v", hop, err)
		}
		c, err := s.AgentCounts(ctx, "b1", "s1", "s2", now.Add(-10*time.Minute), now.Add(-time.Hour))
		if err != nil || c != (hub.AgentCounts{FromRoot: 1, Pair: 0, Backend: 3}) {
			t.Fatalf("counts = %+v %v", c, err)
		}
		c, err = s.AgentCounts(ctx, "b1", "s1", "s2", now.Add(-time.Hour), now.Add(-time.Hour))
		if err != nil || c != (hub.AgentCounts{FromRoot: 2, Pair: 1, Backend: 3}) {
			t.Fatalf("counts = %+v %v", c, err)
		}
	})
}

func TestAgentNames(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		if err := s.SetAgentName(ctx, "b1", "s1", "Build"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAgentName(ctx, "b1", "s2", "build"); !errors.Is(err, hub.ErrAgentNameTaken) {
			t.Fatalf("taken = %v", err)
		}
		if err := s.SetAgentName(ctx, "b2", "s2", "build"); err != nil {
			t.Fatalf("other backend: %v", err)
		}
		if err := s.SetAgentName(ctx, "b1", "s1", "builder"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAgentName(ctx, "b1", "s2", "Build"); err != nil {
			t.Fatalf("freed name: %v", err)
		}
		names, err := s.AgentNames(ctx, "b1")
		if err != nil || len(names) != 2 || names["s1"] != "builder" || names["s2"] != "Build" {
			t.Fatalf("names = %v %v", names, err)
		}
		if err := s.SetAgentName(ctx, "b1", "s1", ""); err != nil {
			t.Fatal(err)
		}
		if names, _ := s.AgentNames(ctx, "b1"); len(names) != 1 {
			t.Fatalf("names after removal = %v", names)
		}
	})
}
