package hub_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func TestOpenTurnPrompt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rows string
		want hub.OpenPrompt
		ok   bool
	}{
		"prompt last":    {`[{"role":"user","text":"a"},{"role":"assistant","text":"b"},{"role":"user","text":"c","submission_id":"x"}]`, hub.OpenPrompt{Text: "c", SubmissionID: "x"}, true},
		"tools after it": {`[{"role":"user","content":"c"},{"role":"assistant","content":"","tool_calls":[{"id":"1"}]},{"role":"tool","content":"r"}]`, hub.OpenPrompt{Text: "c"}, true},
		"reply after it": {`[{"role":"user","text":"a"},{"role":"assistant","text":"b"}]`, hub.OpenPrompt{}, false},
		"note row":       {`[{"role":"user","text":"done","display_kind":"process_complete"}]`, hub.OpenPrompt{}, false},
		"image parts":    {`[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"x"}}]}]`, hub.OpenPrompt{Text: "look", Images: 1}, true},
		"empty":          {`[]`, hub.OpenPrompt{}, false},
	} {
		var rows []json.RawMessage
		if err := json.Unmarshal([]byte(tc.rows), &rows); err != nil {
			t.Fatal(err)
		}
		got, ok := hub.OpenTurnPrompt(rows)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got %+v %v, want %+v %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNewSubmissionID(t *testing.T) {
	t.Parallel()
	id := hub.NewSubmissionID()
	if !hub.FromHub(id) || hub.FromHub(strings.TrimPrefix(id, "hub:")) || id == hub.NewSubmissionID() {
		t.Fatalf("id %q", id)
	}
}
