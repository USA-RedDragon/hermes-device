package store_test

import (
	"context"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const slotWork = "work"

func TestClaudeCodeReports(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		dev := hub.ClaudeCodeSource{ID: "device:d1", Kind: hub.ClaudeCodeDevice, User: userAlice, BackendID: "main"}
		row := func(hour int64, acct string, in int64) hub.ClaudeCodeRow {
			return hub.ClaudeCodeRow{Hour: hour, Account: acct, Model: "claude-opus-5-5", Project: "app", Input: in, Output: 2,
				CacheWrite: 3, CacheWrite1h: 1, CacheRead: 4, Requests: 1}
		}
		if err := s.PutClaudeCodeReport(ctx, dev, hub.ClaudeCodeReport{Hours: []int64{3600, 7200},
			Rows:     []hub.ClaudeCodeRow{row(3600, "", 10), row(7200, "acct-a", 20)},
			Accounts: []hub.ClaudeCodeAccount{{UUID: "acct-a", Label: "alice@example.com"}}}); err != nil {
			t.Fatal(err)
		}
		// The session of hour 3600 got its owner: the hour's row moves.
		if err := s.PutClaudeCodeReport(ctx, dev, hub.ClaudeCodeReport{Hours: []int64{3600},
			Rows: []hub.ClaudeCodeRow{row(3600, "acct-a", 10)}}); err != nil {
			t.Fatal(err)
		}
		herm := hub.ClaudeCodeSource{ID: "hermes:main", Kind: hub.ClaudeCodeHermes, BackendID: "main"}
		if err := s.PutClaudeCodeReport(ctx, herm, hub.ClaudeCodeReport{Since: 3600,
			Rows:     []hub.ClaudeCodeRow{row(10800, "acct-b", 5)},
			Accounts: []hub.ClaudeCodeAccount{{Label: "Claude (work)", Slot: slotWork}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutClaudeCodeReport(ctx, herm, hub.ClaudeCodeReport{Since: 3600, Rows: []hub.ClaudeCodeRow{row(14400, "acct-b", 6)}}); err != nil {
			t.Fatal(err)
		}
		rows, err := s.ClaudeCodeRows(ctx, []string{dev.ID, herm.ID}, 0, 100000)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, r := range rows {
			got[r.Source+" "+r.Account] += r.Input
			if r.CacheWrite1h != 1 || r.Requests != 1 || r.Sidechain {
				t.Errorf("row %+v", r)
			}
		}
		want := map[string]int64{"device:d1 acct-a": 30, "hermes:main acct-b": 6}
		if len(got) != len(want) || got["device:d1 acct-a"] != 30 || got["hermes:main acct-b"] != 6 {
			t.Fatalf("rows %v", got)
		}
		if rows, _ := s.ClaudeCodeRows(ctx, []string{dev.ID}, 7200, 7201); len(rows) != 1 {
			t.Fatalf("range rows %+v", rows)
		}
		srcs, err := s.ClaudeCodeSources(ctx)
		if err != nil || len(srcs) != 2 || srcs[0] != dev || srcs[1] != herm {
			t.Fatalf("sources %+v %v", srcs, err)
		}
		accts, err := s.ClaudeCodeAccounts(ctx, []string{dev.ID, herm.ID})
		if err != nil || len(accts) != 2 || accts[0].Label != "alice@example.com" || accts[1].Slot != slotWork {
			t.Fatalf("accounts %+v %v", accts, err)
		}
		if err := s.SetClaudeAccountName(ctx, userAlice, "acct-a", "old work"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetClaudeAccountName(ctx, userAlice, "acct-a", slotWork); err != nil {
			t.Fatal(err)
		}
		names, err := s.ClaudeAccountNames(ctx, userAlice)
		if err != nil || names["acct-a"] != slotWork || len(names) != 1 {
			t.Fatalf("names %v %v", names, err)
		}
		if names, _ := s.ClaudeAccountNames(ctx, userBob); len(names) != 0 {
			t.Fatalf("bob sees alice's names: %v", names)
		}
		if err := s.SetClaudeAccountName(ctx, userAlice, "acct-a", ""); err != nil {
			t.Fatal(err)
		}
		if names, _ := s.ClaudeAccountNames(ctx, userAlice); len(names) != 0 {
			t.Fatalf("name not removed: %v", names)
		}
	})
}
