package store_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const groupFamily = "family"

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLinkTokenFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	tok, err := s.CreateLinkToken(ctx, 42)
	if err != nil || len(tok) < 40 {
		t.Fatalf("token %q, %v", tok, err)
	}
	if _, ok, _ := s.ConsumeLinkToken(ctx, tok+"x"); ok {
		t.Fatal("wrong token accepted")
	}
	id, ok, err := s.ConsumeLinkToken(ctx, tok)
	if err != nil || !ok || id != 42 {
		t.Fatalf("consume = %d %v %v", id, ok, err)
	}
	if _, ok, _ := s.ConsumeLinkToken(ctx, tok); ok {
		t.Fatal("token used twice")
	}
	if _, ok, _ := s.UserForTelegram(ctx, 42); ok {
		t.Fatal("linked before LinkTelegram")
	}
	if err := s.LinkTelegram(ctx, 42, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkTelegram(ctx, 42, "bob"); err != nil {
		t.Fatal(err)
	}
	if u, ok, err := s.UserForTelegram(ctx, 42); u != "bob" || !ok || err != nil {
		t.Fatalf("user = %q %v %v", u, ok, err)
	}
}

func TestLinkTokenExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_000_000, 0)
	s.SetClock(func() time.Time { return now })
	tok, _ := s.CreateLinkToken(ctx, 7)
	now = now.Add(store.LinkTokenTTL + time.Second)
	if _, ok, _ := s.ConsumeLinkToken(ctx, tok); ok {
		t.Fatal("expired token accepted")
	}
}

func TestGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	if g, err := s.Groups(ctx, "nobody"); g != nil || err != nil {
		t.Fatalf("unknown user groups = %v %v", g, err)
	}
	_ = s.SetGroups(ctx, "alice", []string{"admins", groupFamily})
	_ = s.SetGroups(ctx, "alice", []string{groupFamily})
	if g, _ := s.Groups(ctx, "alice"); !slices.Equal(g, []string{groupFamily}) {
		t.Fatalf("groups = %v", g)
	}
}
