package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

func TestPromptAnswers(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		if _, ok, err := s.PromptAnswer(ctx, "b", "p1"); ok || err != nil {
			t.Fatalf("answer before any = %v %v", ok, err)
		}
		first := hub.PromptAnswer{Choice: hub.ChoiceAlways, Via: hub.AnsweredOnDevice, Device: "laptop"}
		late := hub.PromptAnswer{Choice: hub.ChoiceDeny, Via: hub.AnsweredInTelegram}
		if ok, err := s.RecordAnswer(ctx, "b", "p1", first); !ok || err != nil {
			t.Fatalf("first record = %v %v", ok, err)
		}
		if ok, err := s.RecordAnswer(ctx, "b", "p1", late); ok || err != nil {
			t.Fatalf("late record = %v %v", ok, err)
		}
		if err := s.ForgetAnswer(ctx, "b", "p1", late); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.PromptAnswer(ctx, "b", "p1"); got != first || !ok || err != nil {
			t.Fatalf("answer = %+v %v %v", got, ok, err)
		}
		if _, ok, _ := s.PromptAnswer(ctx, "other", "p1"); ok {
			t.Fatal("found the answer under another backend")
		}
		if ok, err := s.ClaimOutcome(ctx, "b", "p1", true); ok || err != nil {
			t.Fatalf("claim of an answered prompt = %v %v", ok, err)
		}
		if err := s.ForgetAnswer(ctx, "b", "p1", first); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.PromptAnswer(ctx, "b", "p1"); ok {
			t.Fatal("answer kept after forget")
		}
		if _, ok, _ := s.ClaimedOutcome(ctx, "b", "p2"); ok {
			t.Fatal("claimed outcome before a claim")
		}
		if ok, err := s.ClaimOutcome(ctx, "b", "p2", false); !ok || err != nil {
			t.Fatalf("first claim = %v %v", ok, err)
		}
		if ok, _ := s.ClaimOutcome(ctx, "b", "p2", true); ok {
			t.Fatal("second claim succeeded")
		}
		want := hub.ApprovalOutcome{PromptID: "p2", Allowed: false, Via: hub.AnsweredElsewhere}
		if o, ok, err := s.ClaimedOutcome(ctx, "b", "p2"); o != want || !ok || err != nil {
			t.Fatalf("claimed = %+v %v %v", o, ok, err)
		}
		if _, ok, _ := s.PromptAnswer(ctx, "b", "p2"); ok {
			t.Fatal("a claim reads as an answer")
		}
		if ok, _ := s.RecordAnswer(ctx, "b", "p2", first); ok {
			t.Fatal("recorded an answer over a claim")
		}
		if err := s.PruneNotices(ctx, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.ClaimOutcome(ctx, "b", "p2", true); !ok {
			t.Fatal("prune kept a claim")
		}
	})
}
