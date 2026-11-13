package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// A transcript row Hermes gave no time keeps no time: the hub does not
// stamp it with the moment of the read, and a duration of 0 is not kept.
func TestRowConverterLeavesUnknownTimesUnset(t *testing.T) {
	t.Parallel()
	c := newRowConverter()
	res := `{"output":"ok","exit_code":0}`
	rows := []gateway.TranscriptRow{
		{Role: "user", Text: "go"},
		{Role: "assistant", ToolCalls: []struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}{{ID: "c1", Name: "terminal", Args: json.RawMessage(`{"command":"ls"}`)}}},
		{Role: "tool", ToolCallID: "c1", Result: &res, DisplayMetadata: json.RawMessage(`{"tool_outcome":{"duration_ms":0,"is_error":false}}`)},
	}
	evs := make([]hub.SubagentEvent, 0, len(rows))
	for _, r := range rows {
		evs = append(evs, c.row(r)...)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %+v", evs)
	}
	for _, e := range evs {
		if !e.TS.IsZero() {
			t.Fatalf("%s has a time %v, want none", e.Type, e.TS)
		}
	}
	if evs[2].DurationMS != nil {
		t.Fatalf("duration = %d, want unset for 0", *evs[2].DurationMS)
	}
	b, err := json.Marshal(evs[2])
	if err != nil || strings.Contains(string(b), `"ts"`) || strings.Contains(string(b), `"duration_ms"`) {
		t.Fatalf("json = %s %v", b, err)
	}
	// A time Hermes gave and a real duration stay.
	ts := 1_800_000_000.5
	got := c.row(gateway.TranscriptRow{Role: "tool", ToolCallID: "c1", Timestamp: &ts, Result: &res,
		DisplayMetadata: json.RawMessage(`{"tool_outcome":{"duration_ms":1200,"is_error":false}}`)})
	if len(got) != 1 || got[0].TS.UnixMilli() != 1_800_000_000_500 || got[0].DurationMS == nil || *got[0].DurationMS != 1200 {
		t.Fatalf("got = %+v", got)
	}
}
