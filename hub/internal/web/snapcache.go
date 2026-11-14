package web

import (
	"bytes"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// A chat's snapshot is the same bytes for every page that opens it until
// the chat changes (the service hands out one copy per snapshot). Its
// fingerprint, its two halves and its event frames are made once and kept,
// so opening or reloading a chat writes bytes the hub has ready: a big
// chat's snapshot is megabytes of JSON, and parsing it again for each
// viewer kept the first byte of a big chat tens of milliseconds away.
const (
	snapCacheEntries = 8
	snapCacheBytes   = 64 << 20
)

type snapCache struct {
	mu    sync.Mutex
	list  []*snapPrep // most recent last
	bytes int
}

// snapPrep is what one snapshot event makes: the event (by its data's
// memory, sequence, time and provisional mark) and its frames.
type snapPrep struct {
	src  []byte
	seq  int64
	at   int64
	prov bool

	mu sync.Mutex
	fp string
	// head is nil when the snapshot is not split (nothing to leave out).
	head, older []byte
	// wholeF, headF and olderF are the event frames, made on first use.
	wholeF, headF, olderF []byte
	split                 bool
	size                  int
}

func sameEvent(p *snapPrep, e hub.Event) bool {
	return p.seq == e.Seq && p.at == e.At && p.prov == e.Provisional && len(p.src) == len(e.Data) &&
		(len(p.src) == 0 || &p.src[0] == &e.Data[0])
}

// prepared is the kept work for e, made now if there is none: the
// fingerprint and the split.
func (c *snapCache) prepared(e hub.Event) *snapPrep {
	c.mu.Lock()
	for i, p := range c.list {
		if sameEvent(p, e) {
			c.list = append(append(c.list[:i:i], c.list[i+1:]...), p)
			c.mu.Unlock()
			return p
		}
	}
	c.mu.Unlock()
	p := &snapPrep{src: e.Data, seq: e.Seq, at: e.At, prov: e.Provisional}
	if !e.Provisional {
		p.fp = snapFP(e.Data)
	}
	if head, older, ok := splitSnapshot(e.Data); ok {
		p.head, p.older, p.split = trimToolRows(head), older, true
	}
	// The frames come to about the data twice again.
	p.size = 3*len(e.Data) + len(p.head)
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.size > snapCacheBytes/2 {
		return p
	}
	c.list = append(c.list, p)
	c.bytes += p.size
	for len(c.list) > snapCacheEntries || c.bytes > snapCacheBytes {
		c.bytes -= c.list[0].size
		c.list = c.list[1:]
	}
	return p
}

// eventFrame is e as the stream writes it.
func eventFrame(s *Server, e hub.Event, fp string) []byte {
	var b bytes.Buffer
	if !s.writeEventFP(&b, e, fp) {
		return nil
	}
	return b.Bytes()
}

// whole is the frame of the snapshot in one event, tool outputs cut.
func (p *snapPrep) whole(s *Server, e hub.Event) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wholeF == nil {
		data := trimToolRows(e.Data)
		fp := p.fp
		if e.Provisional {
			fp = ""
		}
		p.wholeF = eventFrame(s, hub.Event{Seq: e.Seq, Kind: e.Kind, Data: data, At: e.At, Provisional: e.Provisional}, fp)
	}
	return p.wholeF
}

// headFrame is the first event of a split snapshot: its newest rows. Its
// id is <instance>.0, which no stream covers.
func (p *snapPrep) headFrame(s *Server, e hub.Event) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.headF == nil {
		fp := p.fp
		if e.Provisional {
			fp = ""
		}
		p.headF = eventFrame(s, hub.Event{Kind: e.Kind, Data: p.head, At: e.At, Provisional: e.Provisional}, fp)
	}
	return p.headF
}

// olderFrame is the second event of a split snapshot: the rows the first
// left out.
func (p *snapPrep) olderFrame(s *Server, e hub.Event) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.olderF == nil {
		data, err := json.Marshal(sseEvent{Kind: kindSnapshotOlder, Data: trimToolRows(p.older)})
		if err != nil {
			return nil
		}
		p.olderF = append(append([]byte("id: "+s.instance+"."+strconv.FormatInt(e.Seq, 10)+"\ndata: "), data...), '\n', '\n')
	}
	return p.olderF
}
