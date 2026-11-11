package claudeusage

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// Entry flags.
const (
	flagSidechain uint8 = 1 << iota
	flagHasReq
	flagKeyed
)

// entry is one counted response. Exact and Route are FNV-1a hashes, so the
// table survives a restart: Exact is the dedupe key of the response and
// Route the message id and session, which finds a sidechain's replay of a
// main-thread response. Tokens are capped at 2^32-1 per response.
type entry struct {
	Exact  uint64
	Route  uint64
	AtMS   int64
	Dims   uint32
	Flags  uint8
	Detail uint8
	Tok    [5]uint32
}

func (e *entry) sidechain() bool { return e.Flags&flagSidechain != 0 }
func (e *entry) hasReq() bool    { return e.Flags&flagHasReq != 0 }

func (e *entry) tokens() tokens {
	return tokens{Input: uint64(e.Tok[0]), Output: uint64(e.Tok[1]), CacheWrite: uint64(e.Tok[2]),
		CacheWrite1h: uint64(e.Tok[3]), CacheRead: uint64(e.Tok[4])}
}

func cap32(v uint64) uint32 {
	if v > 1<<32-1 {
		return 1<<32 - 1
	}
	return uint32(v)
}

func hashKey(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// exactKey follows ccusage's usage_dedupe_hash: message and request id
// across sessions; without a request id, message id and session, plus the
// timestamp for ids that are not real Anthropic or OpenAI response ids.
func exactKey(r *record) uint64 {
	switch {
	case r.HasReq:
		return hashKey("r", r.MessageID, r.RequestID)
	case strings.HasPrefix(r.MessageID, "msg_") || strings.HasPrefix(r.MessageID, "resp_"):
		return hashKey("s", r.MessageID, r.Session)
	default:
		return hashKey("t", r.MessageID, r.Session, strconv.FormatInt(r.At.UnixMilli(), 10))
	}
}

func routeKey(messageID, session string) uint64 { return hashKey("route", messageID, session) }

// table holds every counted response and finds duplicates the way
// ccusage's Claude loader does (rust/adapters/claude/src/lib.rs,
// push_deduped_entry).
type table struct {
	entries []entry
	exact   map[uint64]int32
	// routes lists the entries of each message id and session. Most
	// routes have one entry, which route holds; more go to routeMore.
	route     map[uint64]int32
	routeMore map[uint64][]int32
	// aliases are the keys that point at an entry besides its own Exact
	// and Route: the route of a copy in another session, and the exact
	// key of a duplicate that the entry replaced. They are kept so that
	// reindex restores them.
	aliases []alias
}

// alias is an extra key of an entry. Route tells a route key from an
// exact one.
type alias struct {
	Key   uint64
	Idx   int32
	Route bool
}

func newTable() *table {
	return &table{exact: map[uint64]int32{}, route: map[uint64]int32{}, routeMore: map[uint64][]int32{}}
}

func (t *table) addRoute(key uint64, i int32) {
	if j, ok := t.route[key]; !ok {
		t.route[key] = i
		return
	} else if j == i {
		return
	}
	for _, j := range t.routeMore[key] {
		if j == i {
			return
		}
	}
	t.routeMore[key] = append(t.routeMore[key], i)
}

func (t *table) routeEntries(key uint64, fn func(int32) bool) {
	if j, ok := t.route[key]; ok && fn(j) {
		return
	}
	for _, j := range t.routeMore[key] {
		if fn(j) {
			return
		}
	}
}

// add counts r with dims d, or replaces the duplicate it matches when r
// reports more. It reports whether the table changed.
func (t *table) add(r *record, d uint32) bool {
	e := entry{AtMS: r.At.UnixMilli(), Dims: d, Detail: r.detail, Tok: [5]uint32{
		cap32(r.Tokens.Input), cap32(r.Tokens.Output), cap32(r.Tokens.CacheWrite), cap32(r.Tokens.CacheWrite1h), cap32(r.Tokens.CacheRead),
	}}
	if r.Sidechain {
		e.Flags |= flagSidechain
	}
	if r.HasReq {
		e.Flags |= flagHasReq
	}
	if !r.HasID {
		t.entries = append(t.entries, e)
		return true
	}
	e.Flags |= flagKeyed
	e.Exact = exactKey(r)
	e.Route = routeKey(r.MessageID, r.Session)
	idx, found := t.exact[e.Exact]
	if !found {
		t.routeEntries(e.Route, func(j int32) bool {
			x := &t.entries[j]
			if !e.sidechain() && !x.sidechain() {
				return false
			}
			if (!e.hasReq() && !x.hasReq()) || x.AtMS == e.AtMS {
				idx, found = j, true
			}
			return found
		})
	}
	if !found {
		i := int32(len(t.entries)) //nolint:gosec // 35 days hold far fewer responses than 2^31
		t.entries = append(t.entries, e)
		t.exact[e.Exact] = i
		t.addRoute(e.Route, i)
		return true
	}
	if t.entries[idx].Route != e.Route {
		before := len(t.routeMore[e.Route])
		_, had := t.route[e.Route]
		t.addRoute(e.Route, idx)
		if !had || len(t.routeMore[e.Route]) != before {
			t.aliases = append(t.aliases, alias{Key: e.Route, Idx: idx, Route: true})
		}
	}
	if !replaces(&e, &t.entries[idx]) {
		return false
	}
	old := t.entries[idx]
	e.Route = old.Route
	t.entries[idx] = e
	if old.Exact != e.Exact {
		t.aliases = append(t.aliases, alias{Key: old.Exact, Idx: idx})
		if _, ok := t.exact[e.Exact]; !ok {
			t.exact[e.Exact] = idx
		}
	}
	return true
}

// replaces is ccusage's should_replace_deduped_entry: a main-thread copy
// beats a sidechain one, then the larger total, then more detail.
func replaces(c, x *entry) bool {
	if c.sidechain() != x.sidechain() {
		return x.sidechain()
	}
	ct, xt := c.tokens().total(), x.tokens().total()
	if ct != xt {
		return ct > xt
	}
	return c.Detail > x.Detail
}

// prune drops the entries before cutoffMS and rebuilds the indexes.
func (t *table) prune(cutoffMS int64) {
	t.filter(func(e *entry) bool { return e.AtMS >= cutoffMS })
}

// filter keeps the entries keep accepts and rebuilds the indexes.
func (t *table) filter(keep func(*entry) bool) {
	remap := make([]int32, len(t.entries))
	kept := make([]entry, 0, len(t.entries))
	for i, e := range t.entries {
		remap[i] = -1
		if keep(&e) {
			remap[i] = int32(len(kept)) //nolint:gosec // as in add
			kept = append(kept, e)
		}
	}
	aliases := t.aliases[:0]
	for _, a := range t.aliases {
		if a.Idx >= 0 && int(a.Idx) < len(remap) && remap[a.Idx] >= 0 {
			a.Idx = remap[a.Idx]
			aliases = append(aliases, a)
		}
	}
	t.entries, t.aliases = kept, aliases
	t.reindex()
}

func (t *table) reindex() {
	t.exact = make(map[uint64]int32, len(t.entries))
	t.route = make(map[uint64]int32, len(t.entries))
	t.routeMore = map[uint64][]int32{}
	for i := range t.entries {
		e := &t.entries[i]
		if e.Flags&flagKeyed == 0 {
			continue
		}
		if _, ok := t.exact[e.Exact]; !ok {
			t.exact[e.Exact] = int32(i)
		}
		t.addRoute(e.Route, int32(i))
	}
	for _, a := range t.aliases {
		if a.Idx < 0 || int(a.Idx) >= len(t.entries) {
			continue
		}
		if a.Route {
			t.addRoute(a.Key, a.Idx)
		} else if _, ok := t.exact[a.Key]; !ok {
			t.exact[a.Key] = a.Idx
		}
	}
}
