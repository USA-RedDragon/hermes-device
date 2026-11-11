package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/claudecode"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const (
	alice       = "alice"
	bob         = "bob"
	backendMain = "main"
	other       = "other"
	opus        = "claude-opus-5-5"
	acctA       = "aaaaaaaa-1111-4111-8111-111111111111"
	acctB       = "bbbbbbbb-2222-4222-8222-222222222222"
	acctC       = "cccccccc-3333-4333-8333-333333333333"
	desktop     = "desktop"
	laptopID    = "d2"
)

type access struct{}

func (access) Allowed(_ context.Context, user, id string) error {
	if id == backendMain && user == alice || id == other && user == bob {
		return nil
	}
	return hub.ErrForbidden
}

type devices map[string]hub.Device

func (d devices) Device(_ context.Context, id string) (hub.Device, bool, error) {
	x, ok := d[id]
	return x, ok, nil
}

type env struct {
	svc    *claudecode.Service
	st     *store.Store
	now    time.Time
	hermes map[string][]byte
	loc    *time.Location
}

const testPrices = `{
  "claude-opus-5-5": {"input_cost_per_token": 0.000004, "output_cost_per_token": 0.00002, "cache_creation_input_token_cost": 0.000005,
    "cache_creation_input_token_cost_above_1hr": 0.000008, "cache_read_input_token_cost": 0.0000002},
  "claude-sonnet-5-5": {"input_cost_per_token": 0.000002, "output_cost_per_token": 0.00001}
}`

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	prices, err := claudecode.ParsePrices([]byte(testPrices))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, loc: loc, now: time.Date(2026, 11, 3, 15, 0, 0, 0, loc), hermes: map[string][]byte{}}
	e.svc, err = claudecode.New(claudecode.Options{Store: st, Access: access{}, Location: loc, Prices: prices,
		Devices:  devices{"d1": {ID: "d1", User: alice, Name: desktop}, laptopID: {ID: laptopID, User: bob, Name: "laptop"}},
		Backends: []claudecode.Backend{{ID: backendMain, Name: "Main", Hermes: true}, {ID: other, Name: "Other", Hermes: true}},
		Fetch: func(_ context.Context, id string) ([]byte, error) {
			if b, ok := e.hermes[id]; ok {
				return b, nil
			}
			return nil, errors.New("down")
		},
		Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func hourAt(loc *time.Location, y int, m time.Month, d, h int) int64 {
	return time.Date(y, m, d, h, 0, 0, 0, loc).Unix()
}

func row(hour int64, acct, model, project string, in int64) hub.ClaudeCodeRow {
	return hub.ClaudeCodeRow{Hour: hour, Account: acct, Model: model, Project: project, Input: in, Output: 10, CacheWrite: 100,
		CacheWrite1h: 40, CacheRead: 1000, Requests: 1}
}

func body(t *testing.T, r hub.ClaudeCodeReport) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	h := func(d, hr int) int64 { return hourAt(e.loc, 2026, 11, d, hr) }
	if err := e.svc.IngestDevice(ctx, hub.Device{ID: "d1", User: alice, BackendID: backendMain}, body(t, hub.ClaudeCodeReport{
		Hours: []int64{h(3, 9), h(1, 23), h(1, 0)},
		Rows: []hub.ClaudeCodeRow{row(h(3, 9), acctA, opus, "app", 1), row(h(1, 23), acctC, opus, "site", 2),
			row(h(1, 0), "", "claude-sonnet-5-5", "app", 3), row(h(1, 0), acctA, "claude-mystery-1", "app", 4)},
		Accounts: []hub.ClaudeCodeAccount{{UUID: acctA, Label: "alice@example.com"}},
	})); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.IngestDevice(ctx, hub.Device{ID: laptopID, User: bob, BackendID: other}, body(t, hub.ClaudeCodeReport{
		Hours: []int64{h(3, 9)}, Rows: []hub.ClaudeCodeRow{row(h(3, 9), acctB, opus, "bobs", 100)},
	})); err != nil {
		t.Fatal(err)
	}
	e.hermes[backendMain] = body(t, hub.ClaudeCodeReport{Since: h(1, 0) - 30*86400,
		Rows: []hub.ClaudeCodeRow{row(h(2, 12), acctB, opus, "empty-cwd", 5)},
		Accounts: []hub.ClaudeCodeAccount{{UUID: acctB, Label: "Claude (work)", Slot: "work"}, {Label: "Claude (spare)", Slot: "spare"},
			{Label: "old", Slot: "old"}},
	})
	e.hermes[other] = body(t, hub.ClaudeCodeReport{Rows: []hub.ClaudeCodeRow{row(h(2, 12), acctB, opus, "x", 1000)}})
}

func find(u hub.ClaudeCodeUsage, label string) *hub.ClaudeCodeGroup {
	for i := range u.Groups {
		if u.Groups[i].Label == label {
			return &u.Groups[i]
		}
	}
	return nil
}

func TestUsageLeavesOldSlotFolders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(t)
	u, err := e.svc.ClaudeCodeUsage(context.Background(), alice, "7d", "account")
	if err != nil {
		t.Fatal(err)
	}
	if g := find(u, "old"); g != nil {
		t.Fatalf("a folder that no slot names: %+v", g)
	}
	if find(u, "Claude (spare)") == nil {
		t.Fatalf("a slot without login is gone: %+v", u.Groups)
	}
}

func TestUsageByAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(t)
	ctx := context.Background()
	// A removed computer: its hours come again from its new id.
	if err := e.svc.IngestDevice(ctx, hub.Device{ID: "d-gone", User: alice}, body(t, hub.ClaudeCodeReport{
		Rows: []hub.ClaudeCodeRow{row(hourAt(e.loc, 2026, 11, 3, 9), acctA, opus, "app", 1000)}})); err != nil {
		t.Fatal(err)
	}
	u, err := e.svc.ClaudeCodeUsage(ctx, alice, "7d", "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Days) != 7 || u.Days[6] != "2026-11-03" || u.Days[0] != "2026-10-28" || u.TimeZone != "America/Chicago" {
		t.Fatalf("days %v", u.Days)
	}
	// alice sees her computer and the main Hermes; not bob's laptop or the other Hermes.
	if u.Total.Input != 1+2+3+4+5 {
		t.Fatalf("total input %d", u.Total.Input)
	}
	if g := find(u, "alice@example.com"); g == nil || g.Input != 5 || g.AccountUUID != acctA {
		t.Fatalf("named by email: %+v", u.Groups)
	}
	if g := find(u, "Claude (work)"); g == nil || g.Input != 5 {
		t.Fatalf("named by slot: %+v", u.Groups)
	}
	if g := find(u, "Claude (spare)"); g == nil || g.Total != 0 || g.AccountUUID != "" {
		t.Fatalf("slot without login: %+v", u.Groups)
	}
	if g := find(u, "Unknown account"); g == nil || g.Input != 3 {
		t.Fatalf("no owner: %+v", u.Groups)
	}
	var unnamed *hub.ClaudeCodeGroup
	for i := range u.Groups {
		if u.Groups[i].AccountUUID == acctC {
			unnamed = &u.Groups[i]
		}
	}
	if unnamed == nil || unnamed.Label != "" {
		t.Fatalf("unnamed account: %+v", unnamed)
	}
	if err := e.svc.NameClaudeAccount(ctx, alice, acctC, "  old work "); err != nil {
		t.Fatal(err)
	}
	u, _ = e.svc.ClaudeCodeUsage(ctx, alice, "7d", "account")
	if g := find(u, "old work"); g == nil || g.Input != 2 {
		t.Fatalf("named: %+v", u.Groups)
	}
	if u, _ := e.svc.ClaudeCodeUsage(ctx, bob, "7d", "account"); find(u, "old work") != nil || u.Total.Input != 1100 {
		t.Fatalf("bob: %+v", u)
	}
	if u.Unpriced == nil || len(u.Unpriced) != 1 || u.Unpriced[0] != "claude-mystery-1" {
		t.Fatalf("unpriced %v", u.Unpriced)
	}
}

func TestUsageDaysAcrossDST(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(t)
	u, err := e.svc.ClaudeCodeUsage(context.Background(), alice, "7d", "computer")
	if err != nil {
		t.Fatal(err)
	}
	d := find(u, desktop)
	if d == nil || find(u, "Hermes") == nil || len(u.Groups) != 2 {
		t.Fatalf("groups %+v", u.Groups)
	}
	// Nov 1 2026 ends daylight saving time in Chicago: 00:00 and 23:00
	// that day are both Nov 1, index 4.
	perRow := int64(10 + 100 + 1000)
	want := []int64{0, 0, 0, 0, 2 + 3 + 4 + 3*perRow, 0, 1 + perRow}
	for i, v := range want {
		if d.Daily[i] != v {
			t.Fatalf("daily %v, want %v", d.Daily, want)
		}
	}
	if u, _ := e.svc.ClaudeCodeUsage(context.Background(), alice, "today", "model"); len(u.Days) != 1 || find(u, "Opus 5.5") == nil {
		t.Fatalf("today %+v", u)
	}
	if u, _ := e.svc.ClaudeCodeUsage(context.Background(), alice, "mtd", "project"); len(u.Days) != 3 || find(u, "empty-cwd") == nil ||
		find(u, "app").Input != 1+3+4 {
		t.Fatalf("mtd %+v", u)
	}
	if u.Since != "" {
		t.Fatalf("7d since %q", u.Since)
	}
	long, err := e.svc.ClaudeCodeUsage(context.Background(), alice, "90d", "computer")
	if err != nil || len(long.Days) != 90 {
		t.Fatalf("90d %v %+v", err, long.Days)
	}
	firstDay := ""
	for i := range long.Days {
		for _, g := range long.Groups {
			if g.Daily[i] > 0 && firstDay == "" {
				firstDay = long.Days[i]
			}
		}
	}
	if firstDay == "" || long.Since != firstDay {
		t.Fatalf("90d since %q, want %q", long.Since, firstDay)
	}
	for _, q := range [][2]string{{"1y", "account"}, {"7d", "user"}} {
		if _, err := e.svc.ClaudeCodeUsage(context.Background(), alice, q[0], q[1]); !errors.Is(err, hub.ErrBadRange) {
			t.Errorf("%v: %v", q, err)
		}
	}
}

func TestCost(t *testing.T) {
	t.Parallel()
	p, err := claudecode.ParsePrices([]byte(testPrices))
	if err != nil {
		t.Fatal(err)
	}
	opusPrice, ok := p.Find("claude-opus-5-5-fast")
	if !ok {
		t.Fatal("fast variant not priced")
	}
	r := hub.ClaudeCodeRow{Input: 1e6, Output: 1e6, CacheWrite: 3e6, CacheWrite1h: 1e6, CacheRead: 1e6}
	want := 4.0 + 20 + 2*5 + 1*8 + 0.2
	if got := opusPrice.Cost(r); math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost %v, want %v", got, want)
	}
	son, ok := p.Find("claude-sonnet-5-5-20261001")
	if !ok {
		t.Fatal("dated id not priced")
	}
	// Missing rates are Anthropic's multiples of the input rate.
	want = 2 + 10 + 2*2.5 + 1*4 + 0.2
	if got := son.Cost(r); math.Abs(got-want) > 1e-9 {
		t.Fatalf("filled cost %v, want %v", got, want)
	}
	if _, ok := p.Find("gpt-9"); ok {
		t.Fatal("unknown model priced")
	}
	def, err := claudecode.DefaultPrices()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := def.Find("claude-opus-5-5"); !ok {
		t.Fatal("the embedded price list has no claude-opus-5-5")
	}
}

func TestReportChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	d := hub.Device{ID: "d1", User: alice}
	h := hourAt(e.loc, 2026, 11, 3, 9)
	for name, r := range map[string]hub.ClaudeCodeReport{
		"negative":   {Rows: []hub.ClaudeCodeRow{{Hour: h, Input: -1}}},
		"unaligned":  {Rows: []hub.ClaudeCodeRow{{Hour: h + 1}}},
		"future":     {Hours: []int64{h + 30*86400}},
		"1h > write": {Rows: []hub.ClaudeCodeRow{{Hour: h, CacheWrite: 1, CacheWrite1h: 2}}},
		"long model": {Rows: []hub.ClaudeCodeRow{{Hour: h, Model: strings.Repeat("x", 300)}}},
	} {
		if err := e.svc.IngestDevice(ctx, d, body(t, r)); !errors.Is(err, hub.ErrBadClaudeCodeReport) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := e.svc.IngestDevice(ctx, d, []byte("{")); !errors.Is(err, hub.ErrBadClaudeCodeReport) {
		t.Errorf("not JSON: %v", err)
	}
	old := h - 90*86400
	if err := e.svc.IngestDevice(ctx, d, body(t, hub.ClaudeCodeReport{Rows: []hub.ClaudeCodeRow{row(old, acctA, opus, "a", 1)}})); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.st.ClaudeCodeRows(ctx, []string{"device:d1"}, 0, h+3600); len(rows) != 0 {
		t.Fatalf("a row older than the window was stored: %+v", rows)
	}
	for _, n := range []string{"", strings.Repeat("x", 65)} {
		if err := e.svc.NameClaudeAccount(ctx, alice, acctA, n); n != "" && !errors.Is(err, hub.ErrBadName) {
			t.Errorf("name %q: %v", n, err)
		}
	}
	if err := e.svc.NameClaudeAccount(ctx, alice, "bad uuid!", "x"); !errors.Is(err, hub.ErrBadName) {
		t.Errorf("bad uuid: %v", err)
	}
}

func TestModelLabel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"claude-opus-5-5": "Opus 5.5", "claude-sonnet-4-5-20250929": "Sonnet 4.5", "claude-opus-5": "Opus 5",
		"claude-haiku-5-5-fast": "Haiku 5.5 fast", "": "Unknown model", "gpt-9": "gpt-9",
	} {
		if got := claudecode.ModelLabel(in); got != want {
			t.Errorf("ModelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
