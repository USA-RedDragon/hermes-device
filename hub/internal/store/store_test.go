package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/store/storetest"
)

const (
	groupFamily = "family"
	userAlice   = "alice"
	userBob     = "bob"
	deviceName  = "desktop"
)

// backends runs fn against an SQLite store and, when storetest.DSNEnv is
// set, a PostgreSQL store.
func backends(t *testing.T, fn func(t *testing.T, s *store.Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		fn(t, openSQLite(t, filepath.Join(t.TempDir(), "hub.db")))
	})
	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		fn(t, openPostgres(t, storetest.Postgres(t)))
	})
}

func openSQLite(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openPostgres(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLinkTokenFlow(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
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
		if err := s.LinkTelegram(ctx, 42, userAlice); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkTelegram(ctx, 42, userBob); err != nil {
			t.Fatal(err)
		}
		if u, ok, err := s.UserForTelegram(ctx, 42); u != userBob || !ok || err != nil {
			t.Fatalf("user = %q %v %v", u, ok, err)
		}
	})
}

func TestLinkTokenExpires(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_000_000, 0)
		s.SetClock(func() time.Time { return now })
		tok, _ := s.CreateLinkToken(ctx, 7)
		now = now.Add(store.LinkTokenTTL + time.Second)
		if _, ok, _ := s.ConsumeLinkToken(ctx, tok); ok {
			t.Fatal("expired token accepted")
		}
	})
}

func TestGroups(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		if g, err := s.Groups(ctx, "nobody"); g != nil || err != nil {
			t.Fatalf("unknown user groups = %v %v", g, err)
		}
		_ = s.SetGroups(ctx, userAlice, []string{"admins", groupFamily})
		_ = s.SetGroups(ctx, userAlice, []string{groupFamily})
		if g, _ := s.Groups(ctx, userAlice); !slices.Equal(g, []string{groupFamily}) {
			t.Fatalf("groups = %v", g)
		}
	})
}

func TestTelegramState(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		if off, err := s.TelegramOffset(ctx, 123); off != 0 || err != nil {
			t.Fatalf("offset = %d %v", off, err)
		}
		for _, off := range []int64{10, 11} {
			if err := s.SetTelegramOffset(ctx, 123, off); err != nil {
				t.Fatal(err)
			}
		}
		if off, err := s.TelegramOffset(ctx, 123); off != 11 || err != nil {
			t.Fatalf("offset = %d %v", off, err)
		}
		chats := []hub.TelegramChat{
			{ChatID: -500, User: userBob, BackendID: "home", SessionID: "s2"},
			{ChatID: 1, User: userAlice, BackendID: deviceName, SessionID: "s1"},
		}
		for _, c := range chats {
			if err := s.SaveTelegramChat(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		chats[1].SessionID = "s3"
		if err := s.SaveTelegramChat(ctx, chats[1]); err != nil {
			t.Fatal(err)
		}
		got, err := s.TelegramChats(ctx)
		if err != nil || !slices.Equal(got, chats) {
			t.Fatalf("chats = %+v %v", got, err)
		}
	})
}

func TestTelegramGroupsAndTopics(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		at := time.Unix(1_800_000_000, 0)
		if err := s.LinkTelegramGroup(ctx, hub.TelegramGroup{User: userAlice, ChatID: -1001, LinkedAt: at}); err != nil {
			t.Fatal(err)
		}
		topic := hub.TelegramTopic{ChatID: -1001, ThreadID: 5, User: userAlice, BackendID: deviceName, SessionID: "s1", Name: "Fix CI", Seen: at}
		if err := s.SaveTelegramTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.TelegramAttached(ctx, userAlice, deviceName, "s1"); !ok || err != nil {
			t.Fatalf("attached = %v %v", ok, err)
		}
		topic.ThreadID, topic.Name, topic.Closed = 0, "Fix CI · laptop", true
		if err := s.SaveTelegramTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		got, err := s.TelegramTopics(ctx, -1001)
		if err != nil || len(got) != 1 || got[0] != topic {
			t.Fatalf("topics = %+v %v", got, err)
		}
		other := hub.TelegramTopic{ChatID: -1001, ThreadID: 6, User: userAlice, BackendID: deviceName, SessionID: "s2", Name: "Gone", Seen: at}
		if err := s.SaveTelegramTopic(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTelegramTopic(ctx, -1001, deviceName, "s2"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.TelegramTopics(ctx, -1001); len(got) != 1 || got[0].SessionID != "s1" {
			t.Fatalf("topics after a delete = %+v", got)
		}
		if ok, _ := s.TelegramAttached(ctx, userAlice, deviceName, "s1"); ok {
			t.Fatal("a dropped topic counts as attached")
		}
		if id, ok, err := s.TelegramGroup(ctx, userAlice); id != -1001 || !ok || err != nil {
			t.Fatalf("group = %d %v %v", id, ok, err)
		}
		if err := s.SaveTelegramChat(ctx, hub.TelegramChat{ChatID: -1002, User: userAlice, BackendID: deviceName, SessionID: "s9"}); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkTelegramGroup(ctx, hub.TelegramGroup{User: userAlice, ChatID: -1002, LinkedAt: at}); err != nil {
			t.Fatal(err)
		}
		groups, err := s.TelegramGroups(ctx)
		if err != nil || len(groups) != 1 || groups[0].ChatID != -1002 || !groups[0].LinkedAt.Equal(at) {
			t.Fatalf("groups = %+v %v", groups, err)
		}
		if chats, _ := s.TelegramChats(ctx); len(chats) != 0 {
			t.Fatalf("the linked group still has a plain chat: %+v", chats)
		}
		if got, _ := s.TelegramTopics(ctx, -1001); len(got) != 0 {
			t.Fatalf("topics of the old group = %+v", got)
		}
		if err := s.UnlinkTelegramGroup(ctx, -1002); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.TelegramGroup(ctx, userAlice); ok || err != nil {
			t.Fatalf("group after unlink = %v %v", ok, err)
		}
	})
}

func TestReopenKeepsDataAndSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	check := func(t *testing.T, open func() *store.Store) {
		t.Helper()
		if err := open().LinkTelegram(ctx, 9, userAlice); err != nil {
			t.Fatal(err)
		}
		if u, ok, err := open().UserForTelegram(ctx, 9); u != userAlice || !ok || err != nil {
			t.Fatalf("after reopen: %q %v %v", u, ok, err)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "hub.db")
		check(t, func() *store.Store { return openSQLite(t, path) })
	})
	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		dsn := storetest.Postgres(t)
		check(t, func() *store.Store { return openPostgres(t, dsn) })
	})
}

// legacySQLite writes a database the way talaria-hub v0.1.6 did: the three
// tables and no schema_migrations.
func legacySQLite(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE telegram_links (telegram_id INTEGER PRIMARY KEY, user TEXT NOT NULL, linked_at INTEGER NOT NULL)`,
		`CREATE TABLE link_tokens (token_hash TEXT PRIMARY KEY, telegram_id INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE users (name TEXT PRIMARY KEY, groups TEXT NOT NULL, seen_at INTEGER NOT NULL)`,
		`INSERT INTO telegram_links VALUES (42, 'alice', 1000), (43, 'bob', 1001)`,
		`INSERT INTO link_tokens VALUES ('abc', 44, 99999999999)`,
		`INSERT INTO users VALUES ('alice', '["family","admins"]', 1002)`,
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func checkLegacyRows(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	for id, want := range map[int64]string{42: userAlice, 43: userBob} {
		if u, ok, err := s.UserForTelegram(ctx, id); u != want || !ok || err != nil {
			t.Fatalf("link %d = %q %v %v", id, u, ok, err)
		}
	}
	if g, err := s.Groups(ctx, userAlice); !slices.Equal(g, []string{groupFamily, "admins"}) || err != nil {
		t.Fatalf("groups = %v %v", g, err)
	}
}

func TestOpenAdoptsLegacySQLite(t *testing.T) {
	t.Parallel()
	s := openSQLite(t, legacySQLite(t))
	checkLegacyRows(t, s)
	if err := s.SetTelegramOffset(context.Background(), 1, 5); err != nil {
		t.Fatalf("new tables missing: %v", err)
	}
}

func TestImportSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, _, err := openSQLite(t, filepath.Join(t.TempDir(), "hub.db")).ImportSQLite(ctx, "x"); err == nil {
		t.Fatal("import into sqlite accepted")
	}
	s := openPostgres(t, storetest.Postgres(t))
	if n, ran, err := s.ImportSQLite(ctx, filepath.Join(t.TempDir(), "missing.db")); n != 0 || ran || err != nil {
		t.Fatalf("missing file: %d %v %v", n, ran, err)
	}
	if err := s.LinkTelegram(ctx, 43, "carol"); err != nil {
		t.Fatal(err)
	}
	path := legacySQLite(t)
	n, ran, err := s.ImportSQLite(ctx, path)
	if n != 3 || !ran || err != nil {
		t.Fatalf("import = %d %v %v", n, ran, err)
	}
	if u, _, _ := s.UserForTelegram(ctx, 43); u != "carol" {
		t.Fatalf("import overwrote a row PostgreSQL had: %q", u)
	}
	if err := s.LinkTelegram(ctx, 43, userBob); err != nil {
		t.Fatal(err)
	}
	checkLegacyRows(t, s)
	if err := s.LinkTelegram(ctx, 42, "dave"); err != nil {
		t.Fatal(err)
	}
	if n, ran, err := s.ImportSQLite(ctx, path); n != 0 || ran || err != nil {
		t.Fatalf("second import = %d %v %v", n, ran, err)
	}
	if u, _, _ := s.UserForTelegram(ctx, 42); u != "dave" {
		t.Fatalf("second import changed a row: %q", u)
	}
}

func TestSQLiteLockAlwaysHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openSQLite(t, filepath.Join(t.TempDir(), "hub.db"))
	l, held, err := s.TryLock(ctx, 1, time.Second)
	if !held || err != nil {
		t.Fatalf("sqlite lock = %v %v %v", l, held, err)
	}
	select {
	case <-l.Lost():
		t.Fatal("sqlite lock lost")
	case <-time.After(50 * time.Millisecond):
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresLockIsExclusive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storetest.Postgres(t)
	a, b := openPostgres(t, dsn), openPostgres(t, dsn)
	la, held, err := a.TryLock(ctx, 77, 100*time.Millisecond)
	if !held || err != nil {
		t.Fatalf("first lock = %v %v", held, err)
	}
	if _, held, err := b.TryLock(ctx, 77, 100*time.Millisecond); held || err != nil {
		t.Fatalf("second holder got the lock: %v %v", held, err)
	}
	if _, held, err := b.TryLock(ctx, 78, 100*time.Millisecond); !held || err != nil {
		t.Fatalf("other key = %v %v", held, err)
	}
	if err := la.Release(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-la.Lost():
	default:
		t.Fatal("released lock not marked lost")
	}
	lb, held, err := b.TryLock(ctx, 77, 100*time.Millisecond)
	if !held || err != nil {
		t.Fatalf("lock after release = %v %v", held, err)
	}

	terminateLockHolders(t, dsn, 77)
	select {
	case <-lb.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("lost connection not noticed")
	}
	_ = lb.Release(ctx)
	if _, held, err := a.TryLock(ctx, 77, time.Second); !held || err != nil {
		t.Fatalf("lock after holder died = %v %v", held, err)
	}
}

// terminateLockHolders ends the sessions that hold advisory lock key, as
// a lost node or a database failover does.
func terminateLockHolders(t *testing.T, dsn string, key int64) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(context.Background(), `SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND ((classid::bigint << 32) | objid::bigint) = $1`, key).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("terminate lock holders: %d %v", n, err)
	}
}

func TestRelay(t *testing.T) {
	t.Parallel()
	dsn := storetest.Postgres(t)
	a, b := openPostgres(t, dsn), openPostgres(t, dsn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 64)
	go b.Listen(ctx, func(m []byte) { got <- string(m) }, nil)
	if err := a.Publish(ctx, make([]byte, store.MaxRelayMessage+1)); err == nil {
		t.Fatal("oversized message accepted")
	}
	deadline := time.After(10 * time.Second)
	for {
		if err := a.Publish(ctx, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-got:
			if m != "hello" {
				t.Fatalf("got %q", m)
			}
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("no message relayed")
		}
	}
}

func TestDeviceFlow(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		code, _, d := flowApprove(ctx, t, s)
		now = now.Add(5 * time.Second)
		token := flowToken(ctx, t, s, code, d)
		flowSeen(ctx, t, s, d)
		flowRename(ctx, t, s, d)
		flowRevoke(ctx, t, s, d, token)
	})
}

func flowApprove(ctx context.Context, t *testing.T, s *store.Store) (string, string, hub.Device) {
	t.Helper()
	code, user, err := s.StartPairing(ctx, hub.PairingRequest{Name: deviceName, Hostname: deviceName, OS: "linux", Addr: "192.0.2.1"}, time.Minute)
	if err != nil || len(code) < 43 || len(user) != 9 {
		t.Fatalf("start = %q %q %v", code, user, err)
	}
	if res, _ := s.PollPairing(ctx, code, 5*time.Second); res.Status != hub.PairPending {
		t.Fatalf("poll = %+v", res)
	}
	if res, _ := s.PollPairing(ctx, code, 5*time.Second); res.Status != hub.PairSlowDown {
		t.Fatalf("fast poll = %+v", res)
	}
	if _, err := s.ApprovePairing(ctx, "BCDF-GHJK", userAlice, "pc", deviceName); !errors.Is(err, hub.ErrUnknownPairing) {
		t.Fatalf("wrong code = %v", err)
	}
	d, err := s.ApprovePairing(ctx, strings.ToLower(user), userAlice, "pc", deviceName)
	if err != nil || d.User != userAlice || d.Hostname != deviceName || !strings.HasPrefix(d.ID, "d_") {
		t.Fatalf("approve = %+v %v", d, err)
	}
	if _, ok, _ := s.PendingPairing(ctx, user); ok {
		t.Fatal("approved pairing still pending")
	}
	return code, user, d
}

func flowToken(ctx context.Context, t *testing.T, s *store.Store, code string, d hub.Device) string {
	t.Helper()
	res, err := s.PollPairing(ctx, code, 5*time.Second)
	if err != nil || res.Status != hub.PairApproved || !strings.HasPrefix(res.Token, store.TokenPrefix) || res.Device.ID != d.ID {
		t.Fatalf("token poll = %+v %v", res, err)
	}
	if again, _ := s.PollPairing(ctx, code, 5*time.Second); again.Status != hub.PairExpired {
		t.Fatalf("second token poll = %+v", again)
	}
	got, ok, err := s.DeviceByToken(ctx, res.Token)
	if err != nil || !ok || got.ID != d.ID || !got.Paired {
		t.Fatalf("by token = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := s.DeviceByToken(ctx, res.Token+"x"); ok {
		t.Fatal("wrong token accepted")
	}
	return res.Token
}

func flowSeen(ctx context.Context, t *testing.T, s *store.Store, d hub.Device) {
	t.Helper()
	if err := s.SeenDevice(ctx, d.ID, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeenDevice(ctx, d.ID, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	ds, err := s.Devices(ctx, userAlice)
	if err != nil || len(ds) != 1 || ds[0].LastAddr != "198.51.100.7" || ds[0].PrevAddr != "192.0.2.1" || ds[0].AddrChangedAt.IsZero() {
		t.Fatalf("devices = %+v %v", ds, err)
	}
	if err := s.DismissAddressChange(ctx, userAlice, d.ID); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.Devices(ctx, userAlice); !ds[0].AddrChangedAt.IsZero() {
		t.Fatalf("dismissed = %+v", ds[0])
	}
}

func flowRename(ctx context.Context, t *testing.T, s *store.Store, d hub.Device) {
	t.Helper()
	if err := s.RenameDevice(ctx, userBob, d.ID, "mine now"); !errors.Is(err, hub.ErrUnknownDevice) {
		t.Fatalf("bob renamed alice's device: %v", err)
	}
	if err := s.RenameDevice(ctx, userAlice, d.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.Devices(ctx, userAlice); len(ds) != 1 || ds[0].Name != "renamed" {
		t.Fatalf("after rename = %+v", ds)
	}
	_, code, _ := s.StartPairing(ctx, hub.PairingRequest{Name: "second"}, time.Minute)
	other, err := s.ApprovePairing(ctx, code, userAlice, "pc", "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameDevice(ctx, userAlice, other.ID, "renamed"); !errors.Is(err, hub.ErrNameTaken) {
		t.Fatalf("rename to a taken name = %v", err)
	}
	if ok, err := s.RevokeDevice(ctx, userAlice, other.ID); !ok || err != nil {
		t.Fatalf("revoke second = %v %v", ok, err)
	}
	if err := s.RenameDevice(ctx, userAlice, d.ID, deviceName); err != nil {
		t.Fatal(err)
	}
}

func flowRevoke(ctx context.Context, t *testing.T, s *store.Store, d hub.Device, token string) {
	t.Helper()
	_, user2, _ := s.StartPairing(ctx, hub.PairingRequest{Name: deviceName}, time.Minute)
	if _, err := s.ApprovePairing(ctx, user2, userAlice, "pc", deviceName); !errors.Is(err, hub.ErrNameTaken) {
		t.Fatalf("same name = %v", err)
	}
	if _, err := s.ApprovePairing(ctx, user2, userBob, "pc", deviceName); err != nil {
		t.Fatalf("same name for bob = %v", err)
	}
	if ok, _ := s.RevokeDevice(ctx, userBob, d.ID); ok {
		t.Fatal("bob revoked alice's device")
	}
	if ok, err := s.RevokeDevice(ctx, userAlice, d.ID); !ok || err != nil {
		t.Fatalf("revoke = %v %v", ok, err)
	}
	if _, ok, _ := s.DeviceByToken(ctx, token); ok {
		t.Fatal("revoked token accepted")
	}
	if ds, _ := s.Devices(ctx, userAlice); len(ds) != 0 {
		t.Fatalf("after revoke = %+v", ds)
	}
}

func TestPairingExpiresAndDenies(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		code, user, _ := s.StartPairing(ctx, hub.PairingRequest{Name: "a"}, time.Minute)
		if err := s.DenyPairing(ctx, user); err != nil {
			t.Fatal(err)
		}
		if res, _ := s.PollPairing(ctx, code, time.Second); res.Status != hub.PairDenied {
			t.Fatalf("denied = %+v", res)
		}
		code, user, _ = s.StartPairing(ctx, hub.PairingRequest{Name: "b"}, time.Minute)
		if _, err := s.ApprovePairing(ctx, user, userAlice, "pc", "b"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Minute)
		if res, _ := s.PollPairing(ctx, code, time.Second); res.Status != hub.PairExpired {
			t.Fatalf("expired = %+v", res)
		}
		if ds, _ := s.Devices(ctx, userAlice); len(ds) != 0 {
			t.Fatalf("unfinished device kept = %+v", ds)
		}
		for range 3 {
			if _, err := s.CodeFailure(ctx, userAlice, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(2 * time.Minute)
		n, _ := s.CodeFailure(ctx, userAlice, time.Minute)
		if n != 1 {
			t.Fatalf("failures in window = %d", n)
		}
	})
}

func TestDeviceDirs(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		for i := range hub.RecentDirs + 2 {
			now = now.Add(time.Second)
			if err := s.UseDeviceDir(ctx, "d_1", fmt.Sprintf("/home/alice/p%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(time.Second)
		if err := s.UseDeviceDir(ctx, "d_1", "/home/alice/p3"); err != nil {
			t.Fatal(err)
		}
		if err := s.UseDeviceDir(ctx, "d_2", "/home/bob"); err != nil {
			t.Fatal(err)
		}
		got, err := s.DeviceDirs(ctx, "d_1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != hub.RecentDirs || got[0] != "/home/alice/p3" || got[1] != "/home/alice/p9" || slices.Contains(got, "/home/alice/p1") {
			t.Fatalf("dirs = %v", got)
		}
		if got, _ := s.DeviceDirs(ctx, "d_none"); len(got) != 0 {
			t.Fatalf("unknown device dirs = %v", got)
		}
		_, code, err := s.StartPairing(ctx, hub.PairingRequest{Name: deviceName, Hostname: deviceName}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.ApprovePairing(ctx, code, userAlice, "pc", deviceName)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UseDeviceDir(ctx, d.ID, "/home/alice"); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.RevokeDevice(ctx, d.User, d.ID); !ok || err != nil {
			t.Fatalf("revoke = %v, %v", ok, err)
		}
		if got, _ := s.DeviceDirs(ctx, d.ID); len(got) != 0 {
			t.Fatalf("dirs of a revoked device = %v", got)
		}
	})
}

func TestExecLegClose(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		_, user, err := s.StartPairing(ctx, hub.PairingRequest{Name: deviceName}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.ApprovePairing(ctx, user, userAlice, "pc", deviceName)
		if err != nil {
			t.Fatal(err)
		}
		first, second := "leg-a", "leg-b"
		last := func() hub.LegClose {
			t.Helper()
			got, _, err := s.Device(ctx, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			return got.LastClose
		}
		if err := s.ExecLegOpened(ctx, d.ID, first); err != nil {
			t.Fatal(err)
		}
		if err := s.ExecLegOpened(ctx, d.ID, second); err != nil {
			t.Fatal(err)
		}
		if err := s.ExecLegClosed(ctx, d.ID, first, hub.LegClose{Code: 4409, By: hub.CloseByBackend, At: time.Unix(1_800_000_001, 0)}); err != nil {
			t.Fatal(err)
		}
		if c := last(); !c.At.IsZero() {
			t.Fatalf("close of an older leg kept: %+v", c)
		}
		at := time.Unix(1_800_000_002, 0)
		want := hub.LegClose{Code: 4426, Reason: "upgrade them together", By: hub.CloseByBackend, At: at}
		if err := s.ExecLegClosed(ctx, d.ID, second, want); err != nil {
			t.Fatal(err)
		}
		if c := last(); c != want {
			t.Fatalf("close = %+v, want %+v", c, want)
		}
		if err := s.ExecLegOpened(ctx, d.ID, "leg-c"); err != nil {
			t.Fatal(err)
		}
		if c := last(); c != (hub.LegClose{}) {
			t.Fatalf("close after a new open = %+v", c)
		}
	})
}

func TestRelinkDropsThePrivateChatTopics(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		if err := s.LinkTelegram(ctx, 42, userAlice); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkTelegramGroup(ctx, hub.TelegramGroup{User: userAlice, ChatID: 42, LinkedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveTelegramTopic(ctx, hub.TelegramTopic{ChatID: 42, ThreadID: 7, User: userAlice, BackendID: "b", SessionID: "s",
			Seen: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkTelegram(ctx, 42, userAlice); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.TelegramGroup(ctx, userAlice); !ok {
			t.Fatal("linking the same user again dropped the private chat's topics")
		}
		if err := s.LinkTelegram(ctx, 42, userBob); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.TelegramGroup(ctx, userAlice); ok {
			t.Fatal("the old user keeps the private chat")
		}
		if topics, err := s.TelegramTopics(ctx, 42); err != nil || len(topics) != 0 {
			t.Fatalf("topics = %+v %v", topics, err)
		}
	})
}
