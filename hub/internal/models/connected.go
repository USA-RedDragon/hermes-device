package models

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/device"
)

const (
	// connectedLimit bounds each of the reads behind Settings > Models.
	connectedLimit = 20 * time.Second
	// connectedFresh is how long the last answer is served with no new read.
	connectedFresh = 5 * time.Second
	// connectedKeep is how long an answer may stand in when Hermes cannot be read.
	connectedKeep = 5 * time.Minute
)

// seenConnected keeps the last good Settings > Models answer of each
// backend, so the page opens at once from it while a new read runs in the
// background. A change made through the hub drops it (forget), so the next
// read shows that change.
type seenConnected struct {
	mu sync.Mutex
	by map[string]*seenEntry
}

type seenEntry struct {
	data    json.RawMessage
	at      time.Time
	gen     int
	reading bool
}

// forget drops the backend's kept answer and makes a read in flight
// useless.
func (c *seenConnected) forget(backendID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.by[backendID]; e != nil {
		e.data, e.at = nil, time.Time{}
		e.gen++
	}
}

func (c *seenConnected) entry(backendID string) *seenEntry {
	if c.by == nil {
		c.by = map[string]*seenEntry{}
	}
	e := c.by[backendID]
	if e == nil {
		e = &seenEntry{}
		c.by[backendID] = e
	}
	return e
}

// connectedCached answers Settings > Models from the last good answer when
// there is one, and reads again in the background when it is not fresh.
// With none (first open, or after a change), it reads and waits.
func (s *Service) connectedCached(ctx context.Context, r device.Requester, backendID string) (json.RawMessage, error) {
	c := &s.seen
	c.mu.Lock()
	e := c.entry(backendID)
	if e.data != nil && time.Since(e.at) > connectedKeep {
		e.data = nil
	}
	if e.data != nil {
		data, stale := e.data, time.Since(e.at) > connectedFresh
		start := stale && !e.reading
		if start {
			e.reading = true
		}
		gen := e.gen
		c.mu.Unlock()
		if start {
			go s.refreshConnected(context.WithoutCancel(ctx), r, backendID, gen)
		}
		return data, nil
	}
	gen := e.gen
	c.mu.Unlock()
	data, err := s.connected(ctx, r)
	if err != nil {
		return nil, err
	}
	c.store(backendID, gen, data)
	return data, nil
}

func (c *seenConnected) store(backendID string, gen int, data json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entry(backendID); e.gen == gen {
		e.data, e.at = data, time.Now()
	}
}

func (s *Service) refreshConnected(parent context.Context, r device.Requester, backendID string, gen int) {
	// parent is the request's context kept alive past its end.
	ctx, cancel := context.WithTimeout(parent, connectedLimit+5*time.Second)
	defer cancel()
	data, err := s.connected(ctx, r)
	c := &s.seen
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(backendID)
	e.reading = false
	if err == nil && e.gen == gen {
		e.data, e.at = data, time.Now()
	}
}
