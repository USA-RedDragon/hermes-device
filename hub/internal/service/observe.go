package service

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// maxObservedChats bounds the chats whose latest usage a backend keeps.
const maxObservedChats = 500

// observer keeps what a backend's usage events said: the latest rate
// limits of each provider and the latest usage of each chat.
type observer struct {
	mu     sync.Mutex
	since  time.Time
	limits map[string]hub.ProviderLimits
	chats  map[string]hub.LiveUsage
}

func newObserver(now time.Time) *observer {
	return &observer{since: now, limits: map[string]hub.ProviderLimits{}, chats: map[string]hub.LiveUsage{}}
}

type rateBucketJSON struct {
	Limit        float64 `json:"limit"`
	Remaining    float64 `json:"remaining"`
	ResetSeconds float64 `json:"reset_seconds"`
}

func (r rateBucketJSON) bucket() hub.RateBucket {
	return hub.RateBucket{Limit: whole(r.Limit), Remaining: whole(r.Remaining), ResetSeconds: finite(r.ResetSeconds)}
}

// usageFrame is the part of a session.usage payload (gateway/session_usage.py
// usage_frame) and of message.complete's usage that the hub reads. The
// top-level counters of session.usage carry rate_limit, the agent's
// RateLimitState (agent/rate_limit_tracker.py) as a dict.
type usageFrame struct {
	Usage *struct {
		Model       string  `json:"model"`
		ContextUsed float64 `json:"context_used"`
		ContextMax  float64 `json:"context_max"`
		AvgTPS      float64 `json:"avg_tps"`
		AvgLatency  float64 `json:"avg_latency_s"`
	} `json:"usage"`
	Provider  string `json:"provider"`
	RateLimit *struct {
		RequestsMin  rateBucketJSON `json:"requests_min"`
		RequestsHour rateBucketJSON `json:"requests_hour"`
		TokensMin    rateBucketJSON `json:"tokens_min"`
		TokensHour   rateBucketJSON `json:"tokens_hour"`
		CapturedAt   float64        `json:"captured_at"`
		Provider     string         `json:"provider"`
	} `json:"rate_limit"`
}

func whole(f float64) int64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e15 {
		return 0
	}
	return int64(f)
}

func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}

// observe reads one event of a chat. It takes only the observer's lock.
func (o *observer) observe(sessionID, kind string, payload json.RawMessage, now time.Time) {
	if kind != gateway.UsageEvent && kind != hub.KindMessageComplete {
		return
	}
	var f usageFrame
	if json.Unmarshal(payload, &f) != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if rl := f.RateLimit; rl != nil && rl.CapturedAt > 0 && !math.IsInf(rl.CapturedAt, 0) {
		p := rl.Provider
		if p == "" {
			p = f.Provider
		}
		at := time.UnixMilli(int64(rl.CapturedAt * 1000))
		if prev, ok := o.limits[p]; !ok || at.After(prev.At) {
			o.limits[p] = hub.ProviderLimits{Provider: p, At: at, RequestsMin: rl.RequestsMin.bucket(),
				RequestsHour: rl.RequestsHour.bucket(), TokensMin: rl.TokensMin.bucket(), TokensHour: rl.TokensHour.bucket()}
		}
	}
	u := f.Usage
	if u == nil {
		return
	}
	c := o.chats[sessionID]
	c.SessionID, c.At = sessionID, now
	if u.Model != "" {
		c.Model = u.Model
	}
	if f.Provider != "" {
		c.Provider = f.Provider
	}
	if u.ContextMax > 0 {
		c.ContextUsed, c.ContextMax = whole(u.ContextUsed), whole(u.ContextMax)
	}
	if u.AvgTPS > 0 {
		c.TokensPerSec = finite(u.AvgTPS)
	}
	if u.AvgLatency > 0 {
		c.AvgLatencySecs = finite(u.AvgLatency)
	}
	o.chats[sessionID] = c
	if len(o.chats) > maxObservedChats {
		oldest := ""
		for id, x := range o.chats {
			if oldest == "" || x.At.Before(o.chats[oldest].At) {
				oldest = id
			}
		}
		delete(o.chats, oldest)
	}
}

func (o *observer) snapshot() (hub.Observed, time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := hub.Observed{Limits: make([]hub.ProviderLimits, 0, len(o.limits)), Chats: make([]hub.LiveUsage, 0, len(o.chats))}
	for _, l := range o.limits {
		out.Limits = append(out.Limits, l)
	}
	for _, c := range o.chats {
		out.Chats = append(out.Chats, c)
	}
	sort.Slice(out.Limits, func(i, j int) bool { return out.Limits[i].Provider < out.Limits[j].Provider })
	sort.Slice(out.Chats, func(i, j int) bool { return out.Chats[i].At.After(out.Chats[j].At) })
	return out, o.since
}

// Observed returns what this replica saw in the usage events of a
// backend's chats, and since when.
func (s *Service) Observed(ctx context.Context, user, backendID string) (hub.Observed, time.Time, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.Observed{}, time.Time{}, err
	}
	o, since := b.obs.snapshot()
	return o, since, nil
}
