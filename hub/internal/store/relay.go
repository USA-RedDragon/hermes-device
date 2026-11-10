package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// relayChannel is the PostgreSQL NOTIFY channel of Publish and Listen.
const relayChannel = "hermes_hub_events"

// MaxRelayMessage is the largest message Publish takes. PostgreSQL limits a
// NOTIFY payload to just under 8000 bytes.
const MaxRelayMessage = 7900

// ErrRelayMessageSize is returned by Publish for a message over
// MaxRelayMessage.
var ErrRelayMessageSize = errors.New("store: relay message too large")

// Publish sends msg to the Listen of every replica that shares the
// PostgreSQL store, this one too. Messages sent while a listener is
// reconnecting are lost to it.
func (s *Store) Publish(ctx context.Context, msg []byte) error {
	if s.dialect != postgresDialect {
		return ErrNotPostgres
	}
	if len(msg) > MaxRelayMessage {
		return ErrRelayMessageSize
	}
	return s.exec(ctx, `SELECT pg_notify(?, ?)`, relayChannel, string(msg))
}

// Listen calls fn with each published message, in order, until ctx ends. It
// reconnects after an error, which it passes to onErr.
func (s *Store) Listen(ctx context.Context, fn func([]byte), onErr func(error)) {
	delay := time.Second
	for ctx.Err() == nil {
		err := s.listenOnce(ctx, fn, func() { delay = time.Second })
		if ctx.Err() != nil {
			return
		}
		if onErr != nil {
			onErr(err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (s *Store) listenOnce(ctx context.Context, fn func([]byte), listening func()) error {
	if s.dialect != postgresDialect {
		return ErrNotPostgres
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		sc, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("listen: %w: driver connection is %T", driver.ErrBadConn, dc)
		}
		pc := sc.Conn()
		if _, err := pc.Exec(ctx, "LISTEN "+relayChannel); err != nil {
			return fmt.Errorf("listen: %w: %w", driver.ErrBadConn, err)
		}
		listening()
		for {
			n, err := pc.WaitForNotification(ctx)
			if err != nil {
				return fmt.Errorf("listen: %w: %w", driver.ErrBadConn, err)
			}
			fn([]byte(n.Payload))
		}
	})
}
