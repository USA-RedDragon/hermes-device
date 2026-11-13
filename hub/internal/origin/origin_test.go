package origin_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/origin"
)

type wantNotice struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type testCase struct {
	Name string     `json:"name"`
	Row  origin.Row `json:"row"`
	Want struct {
		Show   string      `json:"show"`
		Text   string      `json:"text"`
		Why    string      `json:"why"`
		Notice *wantNotice `json:"notice"`
	} `json:"want"`
}

func loadCases(t *testing.T) []testCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []testCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestClassify(t *testing.T) {
	t.Parallel()
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := origin.Classify(c.Row)
			if got.Show != c.Want.Show || got.Text != c.Want.Text || got.Why != c.Want.Why {
				t.Fatalf("got show %q text %q why %q, want show %q text %q why %q",
					got.Show, got.Text, got.Why, c.Want.Show, c.Want.Text, c.Want.Why)
			}
			switch {
			case c.Want.Notice == nil && got.Notice != nil:
				t.Fatalf("got a card %+v, want none", *got.Notice)
			case c.Want.Notice != nil && got.Notice == nil:
				t.Fatalf("got no card, want %+v", *c.Want.Notice)
			case c.Want.Notice != nil && (got.Notice.Kind != c.Want.Notice.Kind || got.Notice.Title != c.Want.Notice.Title):
				t.Fatalf("got card %q %q, want %q %q", got.Notice.Kind, got.Notice.Title, c.Want.Notice.Kind, c.Want.Notice.Title)
			}
		})
	}
}

// TestTypedNeverLost checks that a row shown as the user's bubble keeps
// text, and that no typed row (a client submission ID) is read by its text.
func TestTypedNeverLost(t *testing.T) {
	t.Parallel()
	for _, c := range loadCases(t) {
		r := c.Row
		r.DisplayKind, r.TypedAsk = "", ""
		r.SubmissionID = "web-typed"
		got := origin.Classify(r)
		want := r.Text
		if c.Want.Show == origin.ShowUser && c.Want.Notice != nil && c.Want.Notice.Kind == "compaction" {
			want = c.Want.Text
		}
		if got.Show != origin.ShowUser || got.Text != want {
			t.Errorf("%s: a typed send shows as %q %q", c.Name, got.Show, got.Text)
		}
	}
}

func TestFromJSON(t *testing.T) {
	t.Parallel()
	r := origin.FromJSON(json.RawMessage(`{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url"},{"type":"text","text":"b"}],` +
		`"display_kind":"","display_metadata":"{\"submission_id\":\"web-1\",\"typed_ask\":\"a\"}"}`))
	if r.Text != "a\nb" || r.SubmissionID != "web-1" || r.TypedAsk != "a" {
		t.Fatalf("got %+v", r)
	}
	r = origin.FromJSON(json.RawMessage(`{"text":"x","submission_id":"tg:1","display_metadata":{"display_text":"d"}}`))
	if r.Text != "x" || r.SubmissionID != "tg:1" || r.DisplayText != "d" {
		t.Fatalf("got %+v", r)
	}
}

// TestScriptMatches runs origin.js against the same cases.
func TestScriptMatches(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	static := filepath.Join("..", "web", "static")
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "run.js"),
		filepath.Join(static, "attach.js"), filepath.Join(static, "notice.js"), filepath.Join(static, "origin.js"), filepath.Join("testdata", "cases.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("origin.js: %v\n%s", err, out)
	}
}
