package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const (
	userCarol = "carol"
	backendB  = "b"
	chatOne   = "s1"
)

func TestDrafts(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.UnixMilli(1_700_000_000_000)
		s.SetClock(func() time.Time { return now })
		if text, at, err := s.Draft(ctx, userCarol, backendB, chatOne); text != "" || !at.IsZero() || err != nil {
			t.Fatalf("draft before any = %q %v %v", text, at, err)
		}
		if err := s.SetDraft(ctx, userCarol, backendB, chatOne, "hello"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if err := s.SetDraft(ctx, userCarol, backendB, chatOne, "hello again"); err != nil {
			t.Fatal(err)
		}
		if text, at, err := s.Draft(ctx, userCarol, backendB, chatOne); text != "hello again" || !at.Equal(now) || err != nil {
			t.Fatalf("draft = %q %v %v", text, at, err)
		}
		if text, _, _ := s.Draft(ctx, "dave", backendB, chatOne); text != "" {
			t.Fatalf("another user's draft = %q", text)
		}
		if err := s.SetDraft(ctx, userCarol, backendB, chatOne, ""); err != nil {
			t.Fatal(err)
		}
		if text, _, _ := s.Draft(ctx, userCarol, backendB, chatOne); text != "" {
			t.Fatalf("draft after delete = %q", text)
		}
	})
}

func TestChatMarks(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.UnixMilli(1_700_000_000_000)
		s.SetClock(func() time.Time { return now })
		marks, base, err := s.ChatMarks(ctx, userCarol)
		if err != nil || len(marks) != 0 || !base.Equal(now) {
			t.Fatalf("first marks = %v %v %v", marks, base, err)
		}
		later := now.Add(time.Hour)
		s.SetClock(func() time.Time { return later })
		if _, base, _ := s.ChatMarks(ctx, userCarol); !base.Equal(now) {
			t.Fatalf("baseline moved to %v", base)
		}
		ref := hub.SessionRef{BackendID: backendB, SessionID: chatOne}
		if err := s.SetPinned(ctx, userCarol, backendB, chatOne, true); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkRead(ctx, userCarol, backendB, chatOne, now.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkRead(ctx, userCarol, backendB, chatOne, now.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		marks, _, _ = s.ChatMarks(ctx, userCarol)
		if m := marks[ref]; !m.Pinned || !m.SeenAt.Equal(now.Add(10*time.Minute)) || m.Unread {
			t.Fatalf("mark after reads = %+v", m)
		}
		if err := s.MarkRead(ctx, userCarol, backendB, chatOne, later.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		marks, _, _ = s.ChatMarks(ctx, userCarol)
		capped := later.Add(time.Minute)
		if m := marks[ref]; !m.SeenAt.Equal(capped) {
			t.Fatalf("read in the future stored %v, want %v", m.SeenAt, capped)
		}
		if err := s.MarkUnread(ctx, userCarol, backendB, chatOne); err != nil {
			t.Fatal(err)
		}
		marks, _, _ = s.ChatMarks(ctx, userCarol)
		if m := marks[ref]; !m.Unread || !m.Pinned || !m.SeenAt.Equal(capped) {
			t.Fatalf("mark after unread = %+v", m)
		}
		if err := s.MarkRead(ctx, userCarol, backendB, chatOne, now); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPinned(ctx, userCarol, backendB, chatOne, false); err != nil {
			t.Fatal(err)
		}
		marks, _, _ = s.ChatMarks(ctx, userCarol)
		if m := marks[ref]; m.Unread || m.Pinned || !m.SeenAt.Equal(capped) {
			t.Fatalf("mark after read and unpin = %+v", m)
		}
		if err := s.MarkUnread(ctx, userCarol, backendB, "s2"); err != nil {
			t.Fatal(err)
		}
		marks, _, _ = s.ChatMarks(ctx, userCarol)
		if m := marks[hub.SessionRef{BackendID: backendB, SessionID: "s2"}]; !m.Unread || !m.SeenAt.IsZero() {
			t.Fatalf("unread mark of an unseen chat = %+v", m)
		}
		if marks, _, _ := s.ChatMarks(ctx, "dave"); len(marks) != 0 {
			t.Fatalf("another user's marks = %v", marks)
		}
	})
}

func TestUnreadRule(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_700_000_000_000)
	cases := []struct {
		name string
		mark hub.ChatMark
		s    hub.Session
		want bool
	}{
		{"never seen, changed before baseline", hub.ChatMark{}, hub.Session{UpdatedAt: base.Add(-time.Hour)}, false},
		{"never seen, changed after baseline", hub.ChatMark{}, hub.Session{UpdatedAt: base.Add(time.Hour)}, true},
		{"seen after change", hub.ChatMark{SeenAt: base.Add(2 * time.Hour)}, hub.Session{UpdatedAt: base.Add(time.Hour)}, false},
		{"changed after seen", hub.ChatMark{SeenAt: base.Add(time.Hour)}, hub.Session{UpdatedAt: base.Add(2 * time.Hour)}, true},
		{"running", hub.ChatMark{SeenAt: base}, hub.Session{UpdatedAt: base.Add(time.Hour), Running: true}, false},
		{"marked by hand", hub.ChatMark{Unread: true, SeenAt: base.Add(time.Hour)}, hub.Session{UpdatedAt: base, Running: true}, true},
	}
	for _, c := range cases {
		if got := c.mark.IsUnread(c.s, base); got != c.want {
			t.Errorf("%s: unread = %v, want %v", c.name, got, c.want)
		}
	}
}
