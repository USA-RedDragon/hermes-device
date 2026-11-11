package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const (
	labelPhone = "Phone"
	fcmX       = "https://fcm.googleapis.com/x"
)

func TestNotifySettings(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		got, err := s.NotifySettings(ctx, userAlice)
		if err != nil || got != hub.DefaultNotifySettings() {
			t.Fatalf("defaults = %+v, %v", got, err)
		}
		want := hub.NotifySettings{Approvals: true, Jobs: true, Push: true, QuietStart: "22:00", QuietEnd: "07:00", TimeZone: "America/Chicago"}
		if err := s.SetNotifySettings(ctx, userAlice, want); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.NotifySettings(ctx, userAlice); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		if got, _ := s.NotifySettings(ctx, userBob); got != hub.DefaultNotifySettings() {
			t.Fatalf("bob got alice's settings: %+v", got)
		}
	})
}

func TestNoticeForMessage(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		id, _, err := s.ClaimNotice(ctx, hub.NoticeRecord{User: userAlice, Key: "k", Kind: hub.NoticeClaudeSignIn, BackendID: "b",
			SessionID: "s", Account: "acc1"})
		if err != nil {
			t.Fatal(err)
		}
		if r, ok, err := s.Notice(ctx, id); !ok || err != nil || r.Account != "acc1" {
			t.Fatalf("notice = %+v %v %v", r, ok, err)
		}
		if err := s.AddNoticeMessage(ctx, id, hub.NoticeMessage{ChatID: 5, MessageID: 9}); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.NoticeForMessage(ctx, 5, 9); got != id || !ok || err != nil {
			t.Fatalf("notice for message = %q %v %v", got, ok, err)
		}
		if _, ok, _ := s.NoticeForMessage(ctx, 5, 10); ok {
			t.Fatal("found a notice for another message")
		}
	})
}

func TestNoticeClaimSettleRemind(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		p := &hub.PromptInfo{ID: "p1", Kind: hub.PromptApproval, Choices: []string{"once", "deny"}}
		r := hub.NoticeRecord{User: userAlice, Key: "prompt:s1:p1", Kind: hub.NoticeApproval, BackendID: "b", SessionID: "s1", PromptID: "p1", Prompt: p}
		id, claimed, err := s.ClaimNotice(ctx, r)
		if err != nil || !claimed || id == "" {
			t.Fatalf("first claim = %q %v %v", id, claimed, err)
		}
		again, claimed, err := s.ClaimNotice(ctx, r)
		if err != nil || claimed || again != id {
			t.Fatalf("second claim = %q %v %v", again, claimed, err)
		}
		r.User = userBob
		if _, claimed, _ := s.ClaimNotice(ctx, r); !claimed {
			t.Fatal("bob's notice was deduplicated against alice's")
		}
		got, ok, err := s.Notice(ctx, id)
		if err != nil || !ok || got.User != userAlice || got.Prompt == nil || got.Prompt.Choices[1] != "deny" {
			t.Fatalf("notice = %+v %v %v", got, ok, err)
		}
		if n, _ := s.SentSince(ctx, userAlice, now.Add(-time.Minute)); n != 1 {
			t.Fatalf("sent since = %d", n)
		}
		if err := s.AddNoticeMessage(ctx, id, hub.NoticeMessage{ChatID: 5, MessageID: 9}); err != nil {
			t.Fatal(err)
		}
		if ms, _ := s.NoticeMessages(ctx, id); len(ms) != 1 || ms[0].MessageID != 9 {
			t.Fatalf("messages = %+v", ms)
		}
		due, err := s.DueReminders(ctx, now, now)
		if err != nil || len(due) != 2 {
			t.Fatalf("due = %+v %v", due, err)
		}
		if err := s.DeferReminder(ctx, id, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if due, _ := s.DueReminders(ctx, now, now); len(due) != 1 {
			t.Fatalf("deferred notice still due: %+v", due)
		}
		if due, _ := s.DueReminders(ctx, now, now.Add(time.Minute)); len(due) != 2 {
			t.Fatalf("deferred notice not due later: %+v", due)
		}
		if ok, _ := s.ClaimReminder(ctx, id); !ok {
			t.Fatal("first reminder claim failed")
		}
		if ok, _ := s.ClaimReminder(ctx, id); ok {
			t.Fatal("second reminder claim worked")
		}
		settled, err := s.SettleNotices(ctx, "b", "s1", []string{"p1"})
		if err != nil || len(settled) != 2 {
			t.Fatalf("settled = %+v %v", settled, err)
		}
		if again, _ := s.SettleNotices(ctx, "b", "s1", []string{"p1"}); len(again) != 0 {
			t.Fatalf("settled twice: %+v", again)
		}
		if due, _ := s.DueReminders(ctx, now, now); len(due) != 0 {
			t.Fatalf("settled notices still due: %+v", due)
		}
		if err := s.PruneNotices(ctx, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Notice(ctx, id); ok {
			t.Fatal("prune kept the notice")
		}
	})
}

func TestPushSubscriptions(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		a, err := s.SavePushSubscription(ctx, hub.PushSubscription{User: userAlice, Endpoint: fcmX, P256dh: "k", Auth: "a", Label: labelPhone})
		if err != nil || a.ID == "" {
			t.Fatalf("save = %+v %v", a, err)
		}
		if list, _ := s.PushSubscriptions(ctx, userBob); len(list) != 0 {
			t.Fatalf("bob sees %+v", list)
		}
		if ok, _ := s.DeletePushSubscription(ctx, userBob, a.ID); ok {
			t.Fatal("bob deleted alice's subscription")
		}
		if _, err := s.SavePushSubscription(ctx, hub.PushSubscription{User: userBob, Endpoint: fcmX, P256dh: "k2", Auth: "a2"}); !errors.Is(err, store.ErrPushInUse) {
			t.Fatalf("bob took alice's endpoint with his own keys: %v", err)
		}
		b, err := s.SavePushSubscription(ctx, hub.PushSubscription{User: userBob, Endpoint: fcmX, P256dh: "k", Auth: "a", Label: labelPhone})
		if err != nil || b.ID != a.ID {
			t.Fatalf("resubscribe = %+v %v", b, err)
		}
		if list, _ := s.PushSubscriptions(ctx, userAlice); len(list) != 0 {
			t.Fatalf("alice keeps the browser bob signed in to: %+v", list)
		}
		list, _ := s.PushSubscriptions(ctx, userBob)
		if len(list) != 1 || list[0].Label != labelPhone {
			t.Fatalf("bob = %+v", list)
		}
		if ok, _ := s.DeletePushSubscription(ctx, userBob, fcmX); !ok {
			t.Fatal("delete by endpoint failed")
		}
	})
}

func TestPresenceAndCronMarks(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		if err := s.SetPresence(ctx, userAlice, "b", "s", true); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.Present(ctx, userAlice, "b", "s", now.Add(-time.Minute)); !ok {
			t.Fatal("not present")
		}
		if ok, _ := s.Present(ctx, userBob, "b", "s", now.Add(-time.Minute)); ok {
			t.Fatal("bob present through alice")
		}
		if ok, _ := s.Present(ctx, userAlice, "b", "s", now.Add(time.Minute)); ok {
			t.Fatal("stale presence counted")
		}
		_ = s.SetPresence(ctx, userAlice, "b", "s", false)
		if ok, _ := s.Present(ctx, userAlice, "b", "s", now.Add(-time.Minute)); ok {
			t.Fatal("presence kept after leaving")
		}
		if _, ok, _ := s.CronMark(ctx, "b", "j"); ok {
			t.Fatal("mark before any")
		}
		_ = s.SetCronMark(ctx, "b", "j", "1")
		_ = s.SetCronMark(ctx, "b", "j", "2")
		if m, ok, _ := s.CronMark(ctx, "b", "j"); !ok || m != "2" {
			t.Fatalf("mark = %q %v", m, ok)
		}
		if err := s.LinkTelegram(ctx, 42, userAlice); err != nil {
			t.Fatal(err)
		}
		if ids, _ := s.TelegramIDs(ctx, userAlice); len(ids) != 1 || ids[0] != 42 {
			t.Fatalf("ids = %v", ids)
		}
		_ = s.SaveTelegramChat(ctx, hub.TelegramChat{ChatID: 42, User: userAlice, BackendID: "b", SessionID: "s"})
		if ok, _ := s.TelegramAttached(ctx, userAlice, "b", "s"); !ok {
			t.Fatal("not attached")
		}
		if ok, _ := s.TelegramAttached(ctx, userBob, "b", "s"); ok {
			t.Fatal("bob attached through alice")
		}
	})
}
