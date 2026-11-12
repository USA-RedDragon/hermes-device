package store_test

import (
	"context"
	"strings"
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

func TestPageMarks(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		ref := hub.SessionRef{BackendID: backendB, SessionID: chatOne}
		if err := s.SetPinned(ctx, userCarol, backendB, chatOne, true); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkPage(ctx, userCarol, backendB, chatOne); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkPage(ctx, userCarol, backendB, "s3"); err != nil {
			t.Fatal(err)
		}
		marks, _, err := s.ChatMarks(ctx, userCarol)
		if err != nil {
			t.Fatal(err)
		}
		if m := marks[ref]; !m.Page || !m.Pinned {
			t.Fatalf("mark after page = %+v", m)
		}
		if m := marks[hub.SessionRef{BackendID: backendB, SessionID: "s3"}]; !m.Page || m.Unread || m.Pinned {
			t.Fatalf("page mark of a new chat = %+v", m)
		}
		if err := s.SetPinned(ctx, userCarol, backendB, "s2", true); err != nil {
			t.Fatal(err)
		}
		marks, _, _ = s.ChatMarks(ctx, userCarol)
		if m := marks[hub.SessionRef{BackendID: backendB, SessionID: "s2"}]; m.Page {
			t.Fatalf("another chat is a page chat = %+v", m)
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

func pinOrder(t *testing.T, s *store.Store, user string) []string {
	t.Helper()
	marks, _, err := s.ChatMarks(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	var list []hub.Session
	for ref, m := range marks {
		if m.Pinned {
			list = append(list, hub.Session{ID: ref.SessionID, BackendID: ref.BackendID})
		}
	}
	hub.PinnedFirst(list, marks)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, x.ID)
	}
	return out
}

func refs(ids ...string) []hub.SessionRef {
	out := make([]hub.SessionRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, hub.SessionRef{BackendID: backendB, SessionID: id})
	}
	return out
}

func TestPinOrder(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		for _, id := range []string{"a", "b", "c"} {
			if err := s.SetPinned(ctx, userCarol, backendB, id, true); err != nil {
				t.Fatal(err)
			}
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "c,b,a" {
			t.Fatalf("new pins go first: %s, want c,b,a", got)
		}
		if err := s.SetPinOrder(ctx, userCarol, refs("b", "a", "c")); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "b,a,c" {
			t.Fatalf("after reorder: %s", got)
		}
		if err := s.SetPinned(ctx, userCarol, backendB, "a", true); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "b,a,c" {
			t.Fatalf("pinning a pinned chat moved it: %s", got)
		}
		if err := s.SetPinned(ctx, userCarol, backendB, "a", false); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "b,c" {
			t.Fatalf("after unpin: %s", got)
		}
		if err := s.SetPinned(ctx, userCarol, backendB, "a", true); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "a,b,c" {
			t.Fatalf("pinned again goes first: %s", got)
		}
		marks, _, _ := s.ChatMarks(ctx, userCarol)
		if err := s.SetPinned(ctx, userCarol, backendB, "c", false); err != nil {
			t.Fatal(err)
		}
		if m := mustMark(t, s, "c"); m.PinPos != 0 || m.Pinned {
			t.Fatalf("unpinned mark = %+v (was %+v)", m, marks[refs("c")[0]])
		}
		if err := s.SetPinOrder(ctx, userCarol, refs("c", "b", "nope", "b")); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "b,a" {
			t.Fatalf("unpinned and unknown names are ignored, unnamed pins follow: %s", got)
		}
	})
}

func mustMark(t *testing.T, s *store.Store, id string) hub.ChatMark {
	t.Helper()
	marks, _, err := s.ChatMarks(context.Background(), userCarol)
	if err != nil {
		t.Fatal(err)
	}
	return marks[hub.SessionRef{BackendID: backendB, SessionID: id}]
}

func TestPinOrderIsPerUser(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		for _, u := range []string{userCarol, "dave"} {
			for _, id := range []string{"a", "b"} {
				if err := s.SetPinned(ctx, u, backendB, id, true); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := s.SetPinOrder(ctx, "dave", refs("a", "b")); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, userCarol), ","); got != "b,a" {
			t.Fatalf("carol's order changed: %s", got)
		}
		if err := s.SetPinOrder(ctx, "erin", refs("a", "b")); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pinOrder(t, s, "dave"), ","); got != "a,b" {
			t.Fatalf("dave's order = %s", got)
		}
	})
}

func TestPinnedFirstKeepsActivityOrder(t *testing.T) {
	t.Parallel()
	at := func(n int64) time.Time { return time.Unix(n, 0) }
	list := []hub.Session{
		{ID: "r-new", BackendID: backendB, UpdatedAt: at(50)},
		{ID: "p-old", BackendID: backendB, UpdatedAt: at(10)},
		{ID: "r-old", BackendID: backendB, UpdatedAt: at(5)},
		{ID: "p-new", BackendID: backendB, UpdatedAt: at(40)},
		{ID: "p-first", BackendID: backendB, UpdatedAt: at(1)},
	}
	marks := map[hub.SessionRef]hub.ChatMark{
		{BackendID: backendB, SessionID: "p-old"}:   {Pinned: true},
		{BackendID: backendB, SessionID: "p-new"}:   {Pinned: true},
		{BackendID: backendB, SessionID: "p-first"}: {Pinned: true, PinPos: -1},
	}
	hub.PinnedFirst(list, marks)
	ids := make([]string, 0, len(list))
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	if got := strings.Join(ids, ","); got != "p-first,p-new,p-old,r-new,r-old" {
		t.Fatalf("order = %s", got)
	}
	if !list[3].UpdatedAt.Equal(at(50)) {
		t.Fatalf("activity time changed")
	}
}
