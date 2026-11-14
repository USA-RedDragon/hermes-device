package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// kindSnapshotSame answers a reconnect whose page already shows the chat's
// transcript (its ?fp= is the snapshot's): nothing to redraw.
const kindSnapshotSame = "snapshot.same"

// kindSnapshotOlder carries the rows a split snapshot left out: the page
// renders them above what it shows, in the background.
const kindSnapshotOlder = "snapshot.older"

// A chat opened fresh gets its snapshot in two events: the newest rows
// first, at least hub.HeadRows rows or hub.HeadBytes bytes in whole turns, so a
// phone draws the end of a big chat before the rest has downloaded, and
// then the older rows. The first event's id is <instance>.0, which no
// stream covers: a reconnect between the two gets a whole snapshot again.

// eventWriter writes one stream's events. split is for a fresh open,
// whose first snapshot goes in two parts.
type eventWriter struct {
	s     *Server
	split bool
	// fp is the transcript the page says it shows (?fp=), "" when it does
	// not.
	fp string
	// before, when set, runs once just before the first bytes are written.
	before func()
}

// emit writes one event, after before.
func (ew *eventWriter) emit(w io.Writer, e hub.Event, fp string) bool {
	if f := ew.before; f != nil {
		ew.before = nil
		f()
	}
	return ew.s.writeEventFP(w, e, fp)
}

// snapFP names a snapshot's transcript: a hash of its messages. It is ""
// for a snapshot that is not an idle chat's (a turn runs, an input waits,
// or an approval is open), whose live state the page must take whole.
func snapFP(data json.RawMessage) string {
	var snap struct {
		Messages json.RawMessage   `json:"messages"`
		Running  bool              `json:"running"`
		Pending  []json.RawMessage `json:"pending"`
		Prompts  []json.RawMessage `json:"prompts"`
	}
	if json.Unmarshal(data, &snap) != nil || len(snap.Messages) == 0 {
		return ""
	}
	if snap.Running || len(snap.Pending) > 0 || len(snap.Prompts) > 0 {
		return ""
	}
	sum := sha256.Sum256(snap.Messages)
	return hex.EncodeToString(sum[:12])
}

func (ew *eventWriter) write(w io.Writer, e hub.Event) bool {
	if e.Kind != hub.KindSnapshot {
		return ew.emit(w, e, "")
	}
	p := ew.s.snaps.prepared(e)
	if !e.Provisional && ew.fp != "" && p.fp == ew.fp {
		// The page shows this transcript already, as it was when it
		// reconnected to a replica that did not know its event id.
		ew.fp, ew.split = "", false
		return ew.emit(w, hub.Event{Seq: e.Seq, Kind: kindSnapshotSame, Data: json.RawMessage("{}")}, p.fp)
	}
	// The page asks for the text of a cut tool output when a card opens;
	// the fingerprint above is of the whole rows.
	if (!ew.split && !e.Provisional) || p.head == nil {
		if !e.Provisional {
			ew.split = false
		}
		return ew.frame(w, p.whole(ew.s, e))
	}
	// The newest rows go first; the older rows follow after the flush.
	if !ew.frame(w, p.headFrame(ew.s, e)) {
		return false
	}
	if e.Provisional {
		// The fresh snapshot follows; its older rows come with it.
		return true
	}
	ew.split = false
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return ew.frame(w, p.olderFrame(ew.s, e))
}

// frame writes one event's bytes, after before.
func (ew *eventWriter) frame(w io.Writer, b []byte) bool {
	if f := ew.before; f != nil {
		ew.before = nil
		f()
	}
	if b == nil {
		return true
	}
	_, err := w.Write(b)
	return err == nil
}

// splitSnapshot cuts a snapshot's messages into the newest whole turns
// (head, the snapshot with "older" set to how many rows it left out) and
// the rest ({"messages": [...]}). ok is false when there is nothing to
// leave out.
func splitSnapshot(data json.RawMessage) (head, older json.RawMessage, ok bool) {
	var snap map[string]json.RawMessage
	if json.Unmarshal(data, &snap) != nil {
		return nil, nil, false
	}
	var msgs []json.RawMessage
	if json.Unmarshal(snap["messages"], &msgs) != nil {
		return nil, nil, false
	}
	cut := hub.HeadCut(msgs)
	if cut <= 0 {
		return nil, nil, false
	}
	keep, err := json.Marshal(msgs[cut:])
	if err != nil {
		return nil, nil, false
	}
	snap["messages"] = keep
	snap["older"] = json.RawMessage(fmt.Sprint(cut))
	head, err = json.Marshal(snap)
	if err != nil {
		return nil, nil, false
	}
	older, err = json.Marshal(map[string][]json.RawMessage{"messages": msgs[:cut]})
	if err != nil {
		return nil, nil, false
	}
	return head, older, true
}
