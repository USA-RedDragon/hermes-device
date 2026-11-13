package web

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// kindSnapshotOlder carries the rows a split snapshot left out: the page
// renders them above what it shows, in the background.
const kindSnapshotOlder = "snapshot.older"

// A chat opened fresh gets its snapshot in two events: the newest rows
// first, at least headRows rows or headBytes bytes in whole turns, so a
// phone draws the end of a big chat before the rest has downloaded, and
// then the older rows. The first event's id is <instance>.0, which no
// stream covers: a reconnect between the two gets a whole snapshot again.
const (
	rowUser      = "user"
	rowAssistant = "assistant"
	headRows     = 40
	headBytes    = 96 << 10
)

// eventWriter writes one stream's events. split is for a fresh open,
// whose first snapshot goes in two parts.
type eventWriter struct {
	s     *Server
	split bool
}

func (ew *eventWriter) write(w io.Writer, e hub.Event) bool {
	if e.Kind != hub.KindSnapshot || (!ew.split && !e.Provisional) {
		return ew.s.writeEvent(w, e)
	}
	head, older, ok := splitSnapshot(e.Data)
	if !ok {
		if e.Provisional {
			return ew.s.writeEvent(w, hub.Event{Kind: e.Kind, Data: e.Data, At: e.At})
		}
		ew.split = false
		return ew.s.writeEvent(w, e)
	}
	if !ew.s.writeEvent(w, hub.Event{Kind: e.Kind, Data: head, At: e.At}) {
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
	cut := headCut(msgs)
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

// headCut is where the head starts: back from the end until headRows rows
// or headBytes bytes, then back to the user message that starts the turn.
func headCut(msgs []json.RawMessage) int {
	i, size := len(msgs), 0
	for i > 0 && len(msgs)-i < headRows && size < headBytes {
		i--
		size += len(msgs[i])
	}
	// A long turn (hundreds of tool calls) is cut at an assistant row, whose
	// tool calls name the tool rows after it, rather than sent whole.
	at, firstAssistant := i, -1
	for i > 0 && size < 4*headBytes {
		r := rowRole(msgs[i])
		if r == rowUser {
			return i
		}
		if r == rowAssistant && firstAssistant < 0 {
			firstAssistant = i
		}
		i--
		size += len(msgs[i])
	}
	if i == 0 {
		return 0
	}
	if firstAssistant >= 0 {
		return firstAssistant
	}
	return at
}

func rowRole(m json.RawMessage) string {
	var r struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(m, &r)
	return r.Role
}
