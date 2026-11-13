package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

// lockSettings make PostgreSQL notice a lock holder that vanished without
// closing its connection (a lost node) within about 15 seconds, so its
// lock frees. They are per session; a server that refuses one keeps its
// default.
//
//nolint:gochecknoglobals // a fixed list
var lockSettings = []string{
	"SET tcp_keepalives_idle = 5",
	"SET tcp_keepalives_interval = 2",
	"SET tcp_keepalives_count = 3",
	"SET tcp_user_timeout = 10000",
}

// Lock is a held session advisory lock on a connection of its own.
type Lock struct {
	conn  *sql.Conn
	key   int64
	lost  chan struct{}
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	lostO sync.Once
}

// TryLock takes the PostgreSQL session advisory lock key without waiting.
// It reports false when another session holds the lock. The lock is checked
// every check interval; Lost is closed when the check fails, as the lock
// may then be gone. On SQLite, which only one process uses, it always
// succeeds and is never lost.
func (s *Store) TryLock(ctx context.Context, key int64, check time.Duration) (*Lock, bool, error) {
	l := &Lock{key: key, lost: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	if s.dialect != postgresDialect {
		close(l.done)
		return l, true, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("lock: %w", err)
	}
	for _, set := range lockSettings {
		_, _ = conn.ExecContext(ctx, set)
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		discard(conn)
		return nil, false, fmt.Errorf("lock: %w", err)
	}
	if !ok {
		_ = conn.Close()
		return nil, false, nil
	}
	l.conn = conn
	// The watcher lives as long as the lock, past ctx.
	go l.watch(context.WithoutCancel(ctx), check)

	return l, true, nil
}

// Lost is closed when the lock may no longer be held.
func (l *Lock) Lost() <-chan struct{} { return l.lost }

func (l *Lock) watch(ctx context.Context, check time.Duration) {
	defer close(l.done)
	t := time.NewTicker(check)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(ctx, check)
			_, err := l.conn.ExecContext(ctx, `SELECT 1`)
			cancel()
			if err != nil {
				l.markLost()
				return
			}
		}
	}
}

func (l *Lock) markLost() {
	l.lostO.Do(func() { close(l.lost) })
}

// Release gives the lock up. It is safe to call more than once and after
// the lock was lost.
func (l *Lock) Release(ctx context.Context) error {
	var err error
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		defer l.markLost()
		if l.conn == nil {
			return
		}
		select {
		case <-l.lost:
			discard(l.conn)
			return
		default:
		}
		var ok bool
		if err = l.conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&ok); err != nil || !ok {
			discard(l.conn)
			if err == nil {
				err = errors.New("lock: not held at release")
			}
			return
		}
		err = l.conn.Close()
	})
	return err
}

// discard closes conn and keeps the pool from reusing it, which ends its
// PostgreSQL session and so every lock and LISTEN it held.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
