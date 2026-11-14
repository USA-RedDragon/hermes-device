package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// A snapshot carries the head of a long tool output and leaves the rest to
// handleToolOutput: a collapsed card shows only its title and status, and
// the tool text was most of a big chat's bytes.
const (
	// outputKeep is the size from which a row's output is cut.
	outputKeep = 4096
	// cutString is the longest string a cut output keeps, in bytes.
	cutString = 600
	// A row that carries an image as a data: URL is never cut: its card
	// shows the image.
	imageMark = "data:image/"
)

// keepWhole are the tools whose rows a card reads in full: a page card
// needs the path, a screenshot card the file.
var keepWhole = map[string]bool{"page_publish": true, hub.ScreenshotTool: true, "browser_vision": true, "vision_analyze": true}

// cutText cuts s to n bytes at a character boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// cutValue cuts every long string of a decoded JSON value.
func cutValue(v any) any {
	switch x := v.(type) {
	case string:
		return cutText(x, cutString)
	case []any:
		for i := range x {
			x[i] = cutValue(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = cutValue(x[k])
		}
	}
	return v
}

// cutOutput shortens one output string: a JSON text keeps its shape (and
// so its exit code, its path, its success flag) with every long string cut,
// any other text keeps its start.
func cutOutput(s string) string {
	if len(s) > 0 && (s[0] == '{' || s[0] == '[') {
		dec := json.NewDecoder(bytes.NewReader([]byte(s)))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) == nil && !dec.More() {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if enc.Encode(cutValue(v)) == nil {
				return string(bytes.TrimRight(buf.Bytes(), "\n"))
			}
		}
	}
	return cutText(s, cutString*2)
}

// trimRow cuts the long output of a tool row; it returns the row as it was
// when there is nothing to cut.
func trimRow(raw json.RawMessage) json.RawMessage {
	if len(raw) < outputKeep || bytes.Contains(raw, []byte(imageMark)) {
		return raw
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	var role, name, tool string
	_ = json.Unmarshal(m["role"], &role)
	if role != "tool" {
		return raw
	}
	_ = json.Unmarshal(m["name"], &name)
	_ = json.Unmarshal(m["tool_name"], &tool)
	if keepWhole[name] || keepWhole[tool] {
		return raw
	}
	size := 0
	for _, k := range []string{"content", "text"} {
		var s string
		if json.Unmarshal(m[k], &s) != nil || len(s) < outputKeep {
			continue
		}
		b, err := json.Marshal(cutOutput(s))
		if err != nil {
			return raw
		}
		m[k] = b
		size = max(size, len(s))
	}
	if size == 0 {
		return raw
	}
	m["output_size"] = json.RawMessage(strconv.Itoa(size))
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// trimToolRows is a snapshot, or the rows of a snapshot.older, with the long
// tool outputs cut (see trimRow). It returns data when nothing is cut.
func trimToolRows(data json.RawMessage) json.RawMessage {
	var snap map[string]json.RawMessage
	if json.Unmarshal(data, &snap) != nil {
		return data
	}
	var rows []json.RawMessage
	if json.Unmarshal(snap["messages"], &rows) != nil {
		return data
	}
	changed := false
	for i, r := range rows {
		if t := trimRow(r); len(t) != len(r) {
			rows[i], changed = t, true
		}
	}
	if !changed {
		return data
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return data
	}
	snap["messages"] = b
	out, err := json.Marshal(snap)
	if err != nil {
		return data
	}
	return out
}

// handleToolOutput sends the full text of a tool row that a snapshot cut:
// ?row= is its row ID, or ?call= its tool call ID. The chat's access rules
// apply as for its events; a row of another chat is not found.
func (s *Server) handleToolOutput(w http.ResponseWriter, r *http.Request) {
	t, ok := s.svc.(hub.ToolOutputs)
	if !ok {
		errorJSON(w, http.StatusNotFound, hub.ErrNoToolOutput.Error())
		return
	}
	q := r.URL.Query()
	row, _ := strconv.ParseInt(q.Get("row"), 10, 64)
	call := q.Get("call")
	if row <= 0 && call == "" {
		errorJSON(w, http.StatusBadRequest, "name a row or a tool call")
		return
	}
	out, err := t.ToolOutput(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), row, call)
	switch {
	case errors.Is(err, hub.ErrNoToolOutput):
		errorJSON(w, http.StatusNotFound, hub.ErrNoToolOutput.Error())
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(out)
}
