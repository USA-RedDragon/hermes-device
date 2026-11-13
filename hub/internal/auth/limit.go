package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Limiter allows at most max events per key in a sliding window. The hub
// keys login and setup attempts by client address, on top of the lockout
// of each account, so one address cannot try many accounts.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewLimiter returns a Limiter of max events per window.
func NewLimiter(maxEvents int, window time.Duration) *Limiter {
	return &Limiter{max: maxEvents, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

// SetClock replaces the limiter's clock. For tests.
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

// Allow records an event for key and reports whether it is within the
// limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	if len(l.hits) > 4096 {
		for k, ts := range l.hits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cut) {
				delete(l.hits, k)
			}
		}
	}
	ts := l.hits[key]
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.max {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}

// Trusted reports whether addr is in one of the prefixes.
func Trusted(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Peer is the address of the request's TCP peer.
func Peer(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// ClientIP is the address of the client: the TCP peer, or, when the peer
// is a trusted proxy, the rightmost address in X-Forwarded-For that is not
// a trusted proxy.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	addr := Peer(r)
	if !Trusted(addr, trusted) {
		return addr
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return addr
		}
		a = a.Unmap()
		if !Trusted(a, trusted) {
			return a
		}
		addr = a
	}
	return addr
}
