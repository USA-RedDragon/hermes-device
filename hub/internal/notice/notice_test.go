package notice_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/notice"
)

// NOTICE_UPDATE=1 go test ./internal/notice rewrites the expected notices
// ("want") in testdata/cases.json from Parse; read the diff before committing.
//
// The texts in testdata/cases.json come from the formatters of Hermes
// (tools/process_registry_notifications.py and gateway/run_notifications.py)
// and from the hub's agent message header. "want" is what the page and the
// bot show; null means the text stays an ordinary user message.
type testCase struct {
	Name string         `json:"name"`
	Text string         `json:"text"`
	Want *notice.Notice `json:"want"`
}

func load(t *testing.T) []testCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []testCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestParse(t *testing.T) {
	t.Parallel()
	cases := load(t)
	if os.Getenv("NOTICE_UPDATE") != "" {
		for i := range cases {
			cases[i].Want = notice.Parse(cases[i].Text)
		}
		b, err := json.MarshalIndent(cases, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "cases.json"), append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := notice.Parse(c.Text)
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("got %+v, want %+v", got, c.Want)
			}
		})
	}
}

func TestParseNeverShowsBoilerplate(t *testing.T) {
	t.Parallel()
	for _, c := range load(t) {
		n := notice.Parse(c.Text)
		if n == nil {
			continue
		}
		b, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"You may have moved on", "INTERNAL NOTIFICATION", "Do not treat it as user input", "peer agent"} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s: shows %q", c.Name, s)
			}
		}
	}
}

func TestLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, want string }{
		{"single completed", "Subagent finished: finish rf9-int for push and re-gate (Completed, 51 min)"},
		{"single failed", "Subagent failed: measurement lane (Failed, 12 min)"},
		{"batch three ok", "3 subagents finished (3 completed, 2 h 2 min in all)"},
		{"process ok", "Background command finished: go test ./... (Exit 0)"},
		{"agent message", "Message from talaria-platform"},
		{"compaction handoff", "Earlier messages were summarized"},
	} {
		for _, k := range load(t) {
			if k.Name == c.name {
				if got := notice.Parse(k.Text).Line(); got != c.want {
					t.Errorf("%s: got %q, want %q", c.name, got, c.want)
				}
			}
		}
	}
}

// notice.js reads the same formats; it must give the same notices.
func TestScriptMatches(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "run.js"), filepath.Join("..", "web", "static", "notice.js"),
		filepath.Join("testdata", "cases.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
