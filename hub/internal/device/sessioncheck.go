package device

import (
	"context"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

type checkEntry struct {
	at   time.Time
	mine bool
}

// sessionCheck looks up session bindings for one leg of a device, and
// remembers for sessionCheckTTL the sessions it found not bound to another
// device.
type sessionCheck struct {
	s   *Server
	b   *backend
	dev hub.Device
	l   *leg

	mu      sync.Mutex
	checked map[string]checkEntry
}

func newSessionCheck(s *Server, b *backend, d hub.Device, l *leg) *sessionCheck {
	return &sessionCheck{s: s, b: b, dev: d, l: l, checked: map[string]checkEntry{}}
}

// check refuses sid when it is bound to another device (through its
// lineage) and, when mine is set, also when it is bound to no device.
func (c *sessionCheck) check(sid string, mine bool) error {
	now := c.s.opts.Now()
	c.mu.Lock()
	e, ok := c.checked[sid]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < sessionCheckTTL && (e.mine || !mine) {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.l.ctx, guardTimeout)
	defer cancel()
	b, bound, err := c.b.plugin.Binding(ctx, sid)
	if err != nil {
		return err
	}
	if bound && b.DeviceID != c.dev.ID {
		return errOtherDevice
	}
	c.mu.Lock()
	c.checked[sid] = checkEntry{at: now, mine: bound}
	c.mu.Unlock()
	if mine && !bound {
		return errNotBound
	}
	return nil
}

// mark remembers that sid is bound to this device.
func (c *sessionCheck) mark(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked[sid] = checkEntry{at: c.s.opts.Now(), mine: true}
}
