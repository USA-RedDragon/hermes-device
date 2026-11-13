// Package leader runs work in exactly one hub replica at a time, the one
// that holds a lock. The hub uses it for the Telegram bot: Telegram lets
// only one process long-poll a bot token.
package leader

import (
	"context"
	"hash/fnv"
	"log/slog"
	"time"
)

// Lease is a held lock.
type Lease interface {
	// Lost is closed when the lock may no longer be held.
	Lost() <-chan struct{}
	// Release gives the lock up.
	Release(ctx context.Context) error
}

// TryLock takes the lock without waiting. It returns a nil Lease when
// another replica holds it.
type TryLock func(ctx context.Context) (Lease, error)

// Key derives an advisory lock key from a name, so different bots and apps
// on one database do not share a lock.
func Key(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("hermes-hub/" + name))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// releaseTimeout bounds giving the lock up.
const releaseTimeout = 5 * time.Second

// Run tries the lock every retry interval until it gets it, then calls lead
// with a context that ends when the lock is lost or ctx ends. When lead
// returns, Run releases the lock and, while ctx lasts, tries again after
// the retry interval. Run returns once ctx ends and lead has returned.
func Run(ctx context.Context, try TryLock, retry time.Duration, log *slog.Logger, lead func(context.Context)) {
	waiting := false
	for ctx.Err() == nil {
		lease, err := try(ctx)
		switch {
		case err != nil:
			log.Warn("Leader lock attempt failed", "error", err.Error())
		case lease == nil:
			if !waiting {
				log.Info("Another replica is the leader, standing by")
				waiting = true
			}
		default:
			waiting = false
			log.Info("Became the leader")
			hold(ctx, lease, log, lead)
		}
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func hold(ctx context.Context, lease Lease, log *slog.Logger, lead func(context.Context)) {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-lease.Lost():
			log.Warn("Lost the leader lock")
			cancel()
		case <-lctx.Done():
		}
	}()
	lead(lctx)
	cancel()
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer rcancel()
	if err := lease.Release(rctx); err != nil {
		log.Warn("Release the leader lock", "error", err.Error())
		return
	}
	log.Info("Released the leader lock")
}
