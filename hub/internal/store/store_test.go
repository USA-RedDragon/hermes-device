package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/store/storetest"
)

const (
	groupFamily = "family"
	userAlice   = "alice"
	userBob     = "bob"
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
			{ChatID: 1, User: userAlice, BackendID: "desktop", SessionID: "s1"},
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

// legacySQLite writes a database the way hermes-hub v0.1.6 did: the three
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
	l, err := s.TryLock(ctx, 1, time.Second)
	if l == nil || err != nil {
		t.Fatalf("sqlite lock = %v %v", l, err)
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
	la, err := a.TryLock(ctx, 77, 100*time.Millisecond)
	if la == nil || err != nil {
		t.Fatalf("first lock = %v %v", la, err)
	}
	if lb, err := b.TryLock(ctx, 77, 100*time.Millisecond); lb != nil || err != nil {
		t.Fatalf("second holder got the lock: %v %v", lb, err)
	}
	if lc, err := b.TryLock(ctx, 78, 100*time.Millisecond); lc == nil || err != nil {
		t.Fatalf("other key = %v %v", lc, err)
	}
	if err := la.Release(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-la.Lost():
	default:
		t.Fatal("released lock not marked lost")
	}
	lb, err := b.TryLock(ctx, 77, 100*time.Millisecond)
	if lb == nil || err != nil {
		t.Fatalf("lock after release = %v %v", lb, err)
	}

	terminateLockHolders(t, dsn, 77)
	select {
	case <-lb.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("lost connection not noticed")
	}
	_ = lb.Release(ctx)
	if la, err := a.TryLock(ctx, 77, time.Second); la == nil || err != nil {
		t.Fatalf("lock after holder died = %v %v", la, err)
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

