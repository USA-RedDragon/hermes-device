package hub_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// The texts in testdata/notes/cases.json come from the code that appends
// the notes (Hermes agent/memory_context.py, agent/skills_index_delta.py,
// agent/surface_switch.py and the subagent-routes plugin), with the
// messages a user could type that must stay. The page runs the same cases
// (internal/web/notes_script_test.go).
func TestCutInjectedNotes(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("testdata", "notes", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Name, Text, Want string }
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range cases {
		if got := hub.CutInjectedNotes(c.Text); got != c.Want {
			t.Errorf("%s: got %q, want %q", c.Name, got, c.Want)
		}
	}
}
