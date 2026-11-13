package leader_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/leader"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/store/storetest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// memLock is one lock shared by several fake replicas.
type memLock struct {
	mu     sync.Mutex
	holder *memLease
}

type memLease struct {
	l    *memLock
	lost chan struct{}
	once sync.Once
}

func (l *memLock) try(context.Context) (leader.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != nil {
		return nil, false, nil
	}
	l.holder = &memLease{l: l, lost: make(chan struct{})}
	return l.holder, true, nil
}

func (m *memLease) Lost() <-chan struct{} { return m.lost }

func (m *memLease) Release(context.Context) error {
	m.l.mu.Lock()
	defer m.l.mu.Unlock()
	if m.l.holder == m {
		m.l.holder = nil
	}
	m.once.Do(func() { close(m.lost) })
	return nil
}

// replica runs leader.Run and counts how many replicas lead at once.
type replica struct {
	cancel context.CancelFunc
	done   chan struct{}
	leads  atomic.Int32
}

func start(t *testing.T, try leader.TryLock, active *atomic.Int32, maxActive *atomic.Int32) *replica {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		leader.Run(ctx, try, 20*time.Millisecond, quiet(), func(ctx context.Context) {
			n := active.Add(1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			r.leads.Add(1)
			<-ctx.Done()
			active.Add(-1)
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOneLeaderAndTakeover(t *testing.T) {
	t.Parallel()
	var lock memLock
	var active, maxActive atomic.Int32
	a := start(t, lock.try, &active, &maxActive)
	waitFor(t, "a to lead", func() bool { return a.leads.Load() == 1 })
	b := start(t, lock.try, &active, &maxActive)
	time.Sleep(100 * time.Millisecond)
	if b.leads.Load() != 0 {
		t.Fatal("b leads while a holds the lock")
	}
	a.cancel()
	<-a.done
	waitFor(t, "b to take over", func() bool { return b.leads.Load() == 1 })

	lock.mu.Lock()
	held := lock.holder
	lock.mu.Unlock()
	held.once.Do(func() { close(held.lost) })
	waitFor(t, "b to lead again after losing its lease", func() bool { return b.leads.Load() == 2 })
	if maxActive.Load() != 1 {
		t.Fatalf("%d replicas led at once", maxActive.Load())
	}
}

func TestKeyDependsOnName(t *testing.T) {
	t.Parallel()
	if leader.Key("telegram/1") == leader.Key("telegram/2") || leader.Key("x") < 0 {
		t.Fatal("bad keys")
	}
}

func TestTakeoverWithPostgres(t *testing.T) {
	t.Parallel()
	dsn := storetest.Postgres(t)
	key := leader.Key("telegram/test")
	var active, maxActive atomic.Int32
	open := func() leader.TryLock {
		s, err := store.OpenPostgres(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return func(ctx context.Context) (leader.Lease, bool, error) {
			l, held, err := s.TryLock(ctx, key, 100*time.Millisecond)
			if !held {
				return nil, false, err
			}
			return l, true, nil
		}
	}
	a := start(t, open(), &active, &maxActive)
	waitFor(t, "a to lead", func() bool { return a.leads.Load() == 1 })
	b := start(t, open(), &active, &maxActive)
	time.Sleep(200 * time.Millisecond)
	if b.leads.Load() != 0 {
		t.Fatal("b leads while a holds the lock")
	}
	stopped := time.Now()
	a.cancel()
	<-a.done
	waitFor(t, "b to take over", func() bool { return b.leads.Load() == 1 })
	if d := time.Since(stopped); d > 2*time.Second {
		t.Fatalf("takeover took %v", d)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("%d replicas led at once", maxActive.Load())
	}
}
