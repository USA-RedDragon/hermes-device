package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/store/storetest"
)

const (
	promptPlan = "Plan-2"
	textReview = "Review this."
	textBobs   = "Bob's own."
	nameReview = "review"
	textPlan   = "Plan it."
)

func TestSavedPrompts(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.UnixMilli(1_700_000_000_000)
		s.SetClock(func() time.Time { return now })
		if ps, err := s.Prompts(ctx, userAlice); err != nil || len(ps) != 0 || ps == nil {
			t.Fatalf("empty library = %#v %v", ps, err)
		}
		for _, p := range []hub.SavedPrompt{{Name: nameReview, Text: textReview}, {Name: promptPlan, Text: textPlan}} {
			if err := s.AddPrompt(ctx, userAlice, p); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AddPrompt(ctx, userAlice, hub.SavedPrompt{Name: "REVIEW", Text: "again"}); !errors.Is(err, hub.ErrPromptExists) {
			t.Fatalf("add a taken name in another case = %v", err)
		}
		if err := s.AddPrompt(ctx, userBob, hub.SavedPrompt{Name: nameReview, Text: textBobs}); err != nil {
			t.Fatalf("bob's prompt with alice's name = %v", err)
		}
		ps, err := s.Prompts(ctx, userAlice)
		if err != nil || len(ps) != 2 || ps[0].Name != promptPlan || ps[1].Text != textReview || !ps[1].UpdatedAt.Equal(now) {
			t.Fatalf("prompts = %+v %v", ps, err)
		}
		if p, ok, err := s.Prompt(ctx, userAlice, "PLAN-2"); !ok || err != nil || p.Name != promptPlan {
			t.Fatalf("prompt by another case = %+v %v %v", p, ok, err)
		}
		if p, ok, _ := s.Prompt(ctx, userBob, nameReview); !ok || p.Text != textBobs {
			t.Fatalf("bob's prompt = %+v %v", p, ok)
		}
		if _, ok, err := s.Prompt(ctx, userBob, "PLAN-2"); ok || err != nil {
			t.Fatalf("bob sees alice's prompt: %v %v", ok, err)
		}
	})
}

func TestSavedPromptsChange(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.UnixMilli(1_700_000_000_000)
		s.SetClock(func() time.Time { return now })
		for _, p := range []hub.SavedPrompt{{Name: nameReview, Text: textReview}, {Name: promptPlan, Text: textPlan}} {
			if err := s.AddPrompt(ctx, userAlice, p); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AddPrompt(ctx, userBob, hub.SavedPrompt{Name: nameReview, Text: textBobs}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if err := s.UpdatePrompt(ctx, userAlice, nameReview, hub.SavedPrompt{Name: "Review", Text: "Review it all."}); err != nil {
			t.Fatal(err)
		}
		if p, _, _ := s.Prompt(ctx, userAlice, nameReview); p.Name != "Review" || p.Text != "Review it all." || !p.UpdatedAt.Equal(now) {
			t.Fatalf("updated prompt = %+v", p)
		}
		if err := s.UpdatePrompt(ctx, userAlice, nameReview, hub.SavedPrompt{Name: "PLAN-2", Text: "x"}); !errors.Is(err, hub.ErrPromptExists) {
			t.Fatalf("rename onto a taken name = %v", err)
		}
		if err := s.UpdatePrompt(ctx, userAlice, nameReview, hub.SavedPrompt{Name: "check", Text: "Check it."}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Prompt(ctx, userAlice, nameReview); ok {
			t.Fatal("the old name is still there after a rename")
		}
		if p, ok, _ := s.Prompt(ctx, userAlice, "check"); !ok || p.Text != "Check it." {
			t.Fatalf("renamed prompt = %+v %v", p, ok)
		}
		if err := s.UpdatePrompt(ctx, userAlice, "nope", hub.SavedPrompt{Name: "nope", Text: "x"}); !errors.Is(err, hub.ErrNoPrompt) {
			t.Fatalf("update a missing prompt = %v", err)
		}
		if err := s.UpdatePrompt(ctx, userAlice, "nope", hub.SavedPrompt{Name: "other", Text: "x"}); !errors.Is(err, hub.ErrNoPrompt) {
			t.Fatalf("rename a missing prompt = %v", err)
		}
		if _, ok, _ := s.Prompt(ctx, userAlice, "other"); ok {
			t.Fatal("a failed rename left a prompt behind")
		}
		if err := s.UpdatePrompt(ctx, userBob, "check", hub.SavedPrompt{Name: "check", Text: "mine"}); !errors.Is(err, hub.ErrNoPrompt) {
			t.Fatalf("bob updates alice's prompt = %v", err)
		}
		if err := s.DeletePrompt(ctx, userBob, "check"); !errors.Is(err, hub.ErrNoPrompt) {
			t.Fatalf("bob deletes alice's prompt = %v", err)
		}
		if err := s.DeletePrompt(ctx, userAlice, "CHECK"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeletePrompt(ctx, userAlice, "check"); !errors.Is(err, hub.ErrNoPrompt) {
			t.Fatalf("delete twice = %v", err)
		}
		if ps, _ := s.Prompts(ctx, userBob); len(ps) != 1 {
			t.Fatalf("bob's prompts after alice's changes = %+v", ps)
		}
	})
}

func TestSavedPromptsLimit(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		for i := range hub.MaxUserPrompts {
			if err := s.AddPrompt(ctx, userCarol, hub.SavedPrompt{Name: fmt.Sprintf("p%d", i), Text: "x"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AddPrompt(ctx, userCarol, hub.SavedPrompt{Name: "one-more", Text: "x"}); !errors.Is(err, hub.ErrTooManyPrompts) {
			t.Fatalf("add past the limit = %v", err)
		}
		if err := s.AddPrompt(ctx, userBob, hub.SavedPrompt{Name: "one-more", Text: "x"}); err != nil {
			t.Fatalf("another user's limit = %v", err)
		}
	})
}

func TestImportSavedPrompts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openPostgres(t, storetest.Postgres(t))
	path := filepath.Join(t.TempDir(), "hub.db")
	old, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.AddPrompt(ctx, userAlice, hub.SavedPrompt{Name: promptPlan, Text: textPlan}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := s.ImportSQLite(ctx, path); !ran || err != nil {
		t.Fatalf("import = %v %v", ran, err)
	}
	if p, ok, err := s.Prompt(ctx, userAlice, promptPlan); !ok || err != nil || p.Text != textPlan {
		t.Fatalf("imported prompt = %+v %v %v", p, ok, err)
	}
}
