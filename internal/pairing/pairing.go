// Package pairing runs the device side of the hub's pairing flow, the OAuth
// 2.0 device authorization grant (RFC 8628) of docs/protocol.md section 7.1.
package pairing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
)

// Errors that end pairing without a token.
var (
	ErrExpired = errors.New("the pairing code expired; run hermes-device login again")
	ErrDenied  = errors.New("pairing was declined on the hub")
)

// DefaultInterval is the poll interval when the hub names none (RFC 8628
// section 3.2).
const DefaultInterval = 5 * time.Second

// SlowDownStep is added to the interval on every slow_down answer.
const SlowDownStep = 5 * time.Second

// Hub is the part of the hub API pairing uses. *hubapi.Client is one.
type Hub interface {
	StartPairing(ctx context.Context, req hubapi.PairRequest) (*hubapi.PairStart, error)
	PollPairing(ctx context.Context, deviceCode string) (*hubapi.PairResult, error)
}

// Pairer runs one pairing.
type Pairer struct {
	Hub Hub
	// Prompt is called once with the hub's answer, to show the user where
	// to confirm the device.
	Prompt func(*hubapi.PairStart)
	// Sleep waits d or until ctx ends. Nil uses a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Pair starts pairing and polls until the user confirms, declines, the
// code expires or ctx ends.
func (p *Pairer) Pair(ctx context.Context, req hubapi.PairRequest) (*hubapi.PairResult, error) {
	sleep, now := p.Sleep, p.Now
	if sleep == nil {
		sleep = sleepCtx
	}
	if now == nil {
		now = time.Now
	}
	start, err := p.Hub.StartPairing(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start pairing: %w", err)
	}
	if p.Prompt != nil {
		p.Prompt(start)
	}
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = DefaultInterval
	}
	var deadline time.Time
	if start.ExpiresIn > 0 {
		deadline = now().Add(time.Duration(start.ExpiresIn) * time.Second)
	}
	for {
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
		if !deadline.IsZero() && now().After(deadline) {
			return nil, ErrExpired
		}
		res, err := p.Hub.PollPairing(ctx, start.DeviceCode)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var he *hubapi.Error
		if !errors.As(err, &he) {
			slog.Warn("Pairing poll failed; retrying", "error", err)
			continue
		}
		switch he.Code {
		case hubapi.PairPending:
		case hubapi.PairSlowDown:
			interval += SlowDownStep
		case hubapi.PairExpired:
			return nil, ErrExpired
		case hubapi.PairDenied:
			return nil, ErrDenied
		default:
			if he.Status >= 500 {
				slog.Warn("Pairing poll failed; retrying", "error", err)
				continue
			}
			return nil, fmt.Errorf("pairing: %w", err)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
