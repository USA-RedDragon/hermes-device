package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// lastFresh is how long a kept answer is served with no new read.
	lastFresh = 2 * time.Second
	// lastKeep is how long a kept answer may stand in for a Hermes that is
	// slow or away; past it the request waits for a live answer.
	lastKeep = 10 * time.Minute
	// lastRefreshLimit bounds a background read.
	lastRefreshLimit = 25 * time.Second
	// lastMax caps the kept answers.
	lastMax = 512
)

// lastAnswers keeps the last good answer of the read-only routes that ask
// Hermes (activity, place, page info, Claude usage, schedules), by user and
// address. A request with one kept is answered from it at once and the
// answer is read again in the background, so a slow or restarting Hermes
// never holds the page, and one slow read never holds another. A request
// with none waits for the live answer. Any change (a non-GET request)
// drops the user's answers, so the next read shows it.
type lastAnswers struct {
	mu sync.Mutex
	by map[string]*lastAnswer
}

type lastAnswer struct {
	body    []byte
	at      time.Time
	gen     int
	reading bool
}

func lastKey(user string, r *http.Request) string { return user + "\x00" + r.URL.RequestURI() }

// forget drops every answer kept for user.
func (c *lastAnswers) forget(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, a := range c.by {
		if len(k) > len(user) && k[:len(user)] == user && k[len(user)] == 0 {
			a.body, a.at = nil, time.Time{}
			a.gen++
		}
	}
}

// capture is a ResponseWriter that holds one answer.
type capture struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.h }
func (c *capture) WriteHeader(code int)        { c.code = code }
func (c *capture) Write(b []byte) (int, error) { return c.buf.Write(b) }

func (s *Server) rememberRead(user string, r *http.Request, h http.HandlerFunc, gen int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lastRefreshLimit)
	defer cancel()
	cp := &capture{h: http.Header{}, code: http.StatusOK}
	h(cp, r.WithContext(ctx))
	c := &s.last
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.by[lastKey(user, r)]
	if a == nil {
		return
	}
	a.reading = false
	if cp.code == http.StatusOK && a.gen == gen {
		a.body, a.at = cp.buf.Bytes(), time.Now()
	}
}

// lastAnswer wraps a GET handler that reads from Hermes.
func (s *Server) lastAnswer(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r)
		key := lastKey(user, r)
		c := &s.last
		c.mu.Lock()
		if c.by == nil {
			c.by = map[string]*lastAnswer{}
		}
		a := c.by[key]
		if a != nil && a.body != nil && time.Since(a.at) <= lastKeep {
			body, start, gen := a.body, time.Since(a.at) > lastFresh && !a.reading, a.gen
			if start {
				a.reading = true
			}
			c.mu.Unlock()
			if start {
				go s.rememberRead(user, r, h, gen)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(body)
			return
		}
		if a == nil {
			if len(c.by) >= lastMax {
				clear(c.by)
			}
			a = &lastAnswer{}
			c.by[key] = a
		}
		gen := a.gen
		c.mu.Unlock()
		cp := &capture{h: http.Header{}, code: http.StatusOK}
		h(cp, r)
		if cp.code == http.StatusOK {
			c.mu.Lock()
			if a2 := c.by[key]; a2 != nil && a2.gen == gen {
				a2.body, a2.at = cp.buf.Bytes(), time.Now()
			}
			c.mu.Unlock()
		}
		for k, v := range cp.h {
			w.Header()[k] = v
		}
		w.WriteHeader(cp.code)
		_, _ = w.Write(cp.buf.Bytes())
	}
}

// warm reads the kept answers a page asks for right after it opens a chat
// (its activity and its place), when there are none yet, so those
// requests are answered from the hub's memory and not after a round trip
// to Hermes. ev is the request that opened the chat's event stream.
func (s *Server) warm(ev *http.Request, user string) {
	base := strings.TrimSuffix(ev.URL.Path, "/events")
	for _, x := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/activity", s.handleActivity}, {"/place", s.handlePlace}} {
		// The same request as the page will make, made here: no network.
		r := ev.Clone(context.WithoutCancel(ev.Context()))
		u := *ev.URL
		u.Path, u.RawPath, u.RawQuery = base+x.path, "", ""
		r.URL, r.RequestURI = &u, u.RequestURI()
		r.SetPathValue("backend", ev.PathValue("backend"))
		r.SetPathValue("session", ev.PathValue("session"))
		key := lastKey(user, r)
		c := &s.last
		c.mu.Lock()
		if c.by == nil {
			c.by = map[string]*lastAnswer{}
		}
		a := c.by[key]
		if a != nil && (a.reading || (a.body != nil && time.Since(a.at) <= lastFresh)) {
			c.mu.Unlock()
			continue
		}
		if a == nil {
			if len(c.by) >= lastMax {
				clear(c.by)
			}
			a = &lastAnswer{}
			c.by[key] = a
		}
		a.reading = true
		gen := a.gen
		c.mu.Unlock()
		go s.rememberRead(user, r, x.h, gen)
	}
}
