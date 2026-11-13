package store_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

const (
	backendHome = "home"
	bobIdentity = "oidc:sub-bob"
)

func TestAccounts(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		if n, err := s.CountAccounts(ctx); err != nil || n != 0 {
			t.Fatalf("count = %d %v", n, err)
		}
		admin := store.Account{Name: userAlice, Display: "Alice", PasswordHash: "h1", Admin: true, Backends: []string{"pc", "pc", backendHome}}
		if err := s.CreateFirstAccount(ctx, admin); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateFirstAccount(ctx, store.Account{Name: userBob}); !errors.Is(err, store.ErrNotFirstAccount) {
			t.Fatalf("second first account: %v", err)
		}
		bob := store.Account{Name: userBob, Email: "Bob@Example.test", Identity: bobIdentity}
		if err := s.CreateAccount(ctx, bob); err != nil {
			t.Fatal(err)
		}
		for _, dup := range []store.Account{{Name: userAlice}, {Name: userCarol, Identity: bobIdentity}} {
			if err := s.CreateAccount(ctx, dup); !errors.Is(err, store.ErrAccountExists) {
				t.Fatalf("duplicate %+v: %v", dup, err)
			}
		}
		a, ok, err := s.Account(ctx, userAlice)
		if err != nil || !ok || !a.Admin || a.Display != "Alice" || !slices.Equal(a.Backends, []string{backendHome, "pc"}) || a.Epoch != 0 {
			t.Fatalf("alice = %+v %v %v", a, ok, err)
		}
		if b, ok, err := s.AccountByIdentity(ctx, bobIdentity); err != nil || !ok || b.Name != userBob {
			t.Fatalf("by identity = %+v %v %v", b, ok, err)
		}
		if _, ok, _ := s.AccountByEmail(ctx, "bob@example.test"); ok {
			t.Fatal("email matched an account with a bound identity")
		}
		if err := s.CreateAccount(ctx, store.Account{Name: userCarol, Email: "Carol@Example.test"}); err != nil {
			t.Fatal(err)
		}
		if c, ok, err := s.AccountByEmail(ctx, " carol@example.TEST "); err != nil || !ok || c.Name != userCarol {
			t.Fatalf("by email = %+v %v %v", c, ok, err)
		}
		if names, err := s.AccountNames(ctx); err != nil || !slices.Equal(names, []string{userAlice, userBob, userCarol}) {
			t.Fatalf("names = %v %v", names, err)
		}
		testAccountUpdates(t, s)
	})
}

func testAccountUpdates(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	on, hash, grants := true, "h2", []string{backendHome}
	if err := s.UpdateAccount(ctx, userBob, store.AccountUpdate{Disabled: &on, Backends: &grants}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccount(ctx, userAlice, store.AccountUpdate{PasswordHash: &hash}); err != nil {
		t.Fatal(err)
	}
	dup := bobIdentity
	if err := s.UpdateAccount(ctx, userCarol, store.AccountUpdate{Identity: &dup}); !errors.Is(err, store.ErrAccountExists) {
		t.Fatalf("bind a taken identity: %v", err)
	}
	if err := s.UpdateAccount(ctx, "nobody", store.AccountUpdate{Revoke: true}); !errors.Is(err, store.ErrNoAccount) {
		t.Fatalf("update unknown: %v", err)
	}
	all, err := s.Accounts(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("accounts = %+v %v", all, err)
	}
	if all[0].PasswordHash != "h2" || all[0].Epoch != 1 || !all[1].Disabled || all[1].Epoch != 1 || !slices.Equal(all[1].Backends, grants) {
		t.Fatalf("after update = %+v", all)
	}
	if g, err := s.Grants(ctx, userBob); err != nil || !slices.Equal(g, grants) {
		t.Fatalf("grants = %v %v", g, err)
	}
	if err := s.DeleteAccount(ctx, userBob); err != nil {
		t.Fatal(err)
	}
	if g, err := s.Grants(ctx, userBob); err != nil || len(g) != 0 {
		t.Fatalf("grants after delete = %v %v", g, err)
	}
	if err := s.DeleteAccount(ctx, userBob); !errors.Is(err, store.ErrNoAccount) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestFirstAccountOnce(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			wg.Go(func() { errs <- s.CreateFirstAccount(ctx, store.Account{Name: name}) })
		}
		wg.Wait()
		close(errs)
		ok := 0
		for err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, store.ErrNotFirstAccount):
				t.Error(err)
			}
		}
		if n, _ := s.CountAccounts(ctx); ok != 1 || n != 1 {
			t.Fatalf("%d first accounts made, %d stored", ok, n)
		}
	})
}

func TestLoginLockout(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		if err := s.CreateAccount(ctx, store.Account{Name: userAlice, PasswordHash: "h"}); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			locked, err := s.LoginFailed(ctx, userAlice, 3, time.Minute)
			if err != nil || locked != (i == 2) {
				t.Fatalf("failure %d: locked %v %v", i, locked, err)
			}
		}
		a, _, _ := s.Account(ctx, userAlice)
		if !a.Locked(now) || a.Locked(now.Add(time.Minute)) || a.Failures != 0 {
			t.Fatalf("lock = %+v", a)
		}
		if _, err := s.LoginFailed(ctx, userAlice, 3, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := s.LoginSucceeded(ctx, userAlice); err != nil {
			t.Fatal(err)
		}
		if a, _, _ := s.Account(ctx, userAlice); a.Failures != 0 {
			t.Fatalf("failures after success = %d", a.Failures)
		}
		if locked, err := s.LoginFailed(ctx, "nobody", 3, time.Minute); err != nil || locked {
			t.Fatalf("unknown account: %v %v", locked, err)
		}
		hash := "new"
		if _, err := s.LoginFailed(ctx, userAlice, 1, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateAccount(ctx, userAlice, store.AccountUpdate{PasswordHash: &hash}); err != nil {
			t.Fatal(err)
		}
		if a, _, _ := s.Account(ctx, userAlice); a.Locked(now) {
			t.Fatal("a new password did not unlock the account")
		}
	})
}

func TestAuthSecrets(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		a, err := s.SessionSecret(ctx)
		if err != nil || len(a) < 32 {
			t.Fatalf("secret = %d chars, %v", len(a), err)
		}
		if b, err := s.SessionSecret(ctx); err != nil || b != a {
			t.Fatal("session secret changed")
		}
	})
}
