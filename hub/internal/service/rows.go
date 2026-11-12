package service

import (
	"bytes"
	"encoding/json"
)

// unshownKeys are the fields of a transcript row that no viewer reads:
// provider reasoning blobs (often encrypted) and Hermes's own bookkeeping.
// A snapshot leaves them out, which makes a long chat with tools about half
// as large.
func unshownKeys() [7]string {
	return [...]string{"reasoning_details", "codex_reasoning_items", "codex_message_items", "message_uid",
		"_tool_call_uids", "_tool_call_uid", "_db_persisted"}
}

// viewRows drops the unshownKeys from rows. A row it cannot read stays as
// it is.
func viewRows(rows []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(rows))
	for i, r := range rows {
		out[i] = viewRow(r)
	}
	return out
}

func viewRow(r json.RawMessage) json.RawMessage {
	keys := unshownKeys()
	found := false
	for _, k := range keys {
		if bytes.Contains(r, []byte(`"`+k+`"`)) {
			found = true
			break
		}
	}
	if !found {
		return r
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(r, &m) != nil || m == nil {
		return r
	}
	for _, k := range keys {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return r
	}
	return b
}
