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
		return ew.s.writeEvent(w, e)
	}
	var fp string
	if !e.Provisional {
		fp = snapFP(e.Data)
		if ew.fp != "" && fp == ew.fp {
			// The page shows this transcript already, as it was when it
			// reconnected to a replica that did not know its event id.
			ew.fp, ew.split = "", false
			return ew.s.writeEventFP(w, hub.Event{Seq: e.Seq, Kind: kindSnapshotSame, Data: json.RawMessage("{}")}, fp)
		}
	}
	// The page asks for the text of a cut tool output when a card opens;
	// the fingerprint above is of the whole rows.
	e.Data = trimToolRows(e.Data)
	if !ew.split && !e.Provisional {
		return ew.s.writeEventFP(w, e, fp)
	}
	head, older, ok := splitSnapshot(e.Data)
	if !ok {
		if e.Provisional {
			return ew.s.writeEvent(w, hub.Event{Kind: e.Kind, Data: e.Data, At: e.At, Provisional: true})
		}
		ew.split = false
		return ew.s.writeEventFP(w, e, fp)
	}
	if !ew.s.writeEventFP(w, hub.Event{Kind: e.Kind, Data: head, At: e.At, Provisional: e.Provisional}, fp) {
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
	data, err := json.Marshal(sseEvent{Kind: kindSnapshotOlder, Data: older})
	if err != nil {
		return true
	}
	_, err = fmt.Fprintf(w, "id: %s.%d\ndata: %s\n\n", ew.s.instance, e.Seq, data)
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
