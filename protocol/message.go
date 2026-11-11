package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ErrAmbiguousMessage is returned for an exec leg message whose type could
// read differently to Go and Python decoders.
var ErrAmbiguousMessage = errors.New("not a JSON object with at most one string type")

// MessageType reads the type of an exec leg message strictly, so that the
// hub, the device (Go, case-insensitive field names, first value kept for
// a null) and the plugin (Python, exact names, last value kept) agree on
// it. It fails for anything but a JSON object, for a member name that
// appears twice, for a member other than "type" whose name equals "type"
// without regard to case, and for a "type" that is not a string. A message
// without "type" has type "". It returns the members by name.
func MessageType(data []byte) (string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", nil, ErrAmbiguousMessage
	}
	m := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", nil, ErrAmbiguousMessage
		}
		key, ok := tok.(string)
		if !ok {
			return "", nil, ErrAmbiguousMessage
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return "", nil, ErrAmbiguousMessage
		}
		if _, dup := m[key]; dup || (key != "type" && strings.EqualFold(key, "type")) {
			return "", nil, ErrAmbiguousMessage
		}
		m[key] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", nil, ErrAmbiguousMessage
	}
	if _, err := dec.Token(); err == nil {
		return "", nil, ErrAmbiguousMessage
	}
	raw, ok := m["type"]
	if !ok {
		return "", m, nil
	}
	var t string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &t) != nil {
		return "", nil, ErrAmbiguousMessage
	}
	return t, m, nil
}
