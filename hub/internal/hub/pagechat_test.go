package hub_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const (
	scopeWindow = "window"
	scopeTab    = "tab"
	exampleURL  = "https://example.com/"
)

func TestWithPageContext(t *testing.T) {
	t.Parallel()
	p := hub.PageContext{Scope: scopeWindow, Tabs: []hub.PageTab{
		{ID: 12, Title: "Docs\nfor   aws", URL: "https://example.com/docs", Active: true},
		{ID: 13, Title: strings.Repeat("x", 300), URL: "http://example.com/pr/1"},
	}, Selection: &hub.PageSelection{TabID: 12, Text: "  stale rule snapshot  "}}
	out, err := hub.WithPageContext("Does this explain it?\n", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Does this explain it?\n\n" + hub.PageContextHead + " ",
		"The window's 2 tabs:\n- tab_id 12, active: Docs for aws <https://example.com/docs>\n",
		"- tab_id 13: " + strings.Repeat("x", hub.MaxPageTitle) + "... <http://example.com/pr/1>\n",
		"Text the user selected in tab_id 12:\nstale rule snapshot\n",
		"browser_snapshot",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block lacks %q:\n%s", want, out)
		}
	}
	ids := regexp.MustCompile(`<untrusted-page-context id="([0-9a-f]{16})">\n[\s\S]*\n</untrusted-page-context id="([0-9a-f]{16})">$`).FindStringSubmatch(out)
	if ids == nil || ids[1] != ids[2] {
		t.Errorf("untrusted block = %q", out)
	}
	if got := hub.CutPageContext(out); got != "Does this explain it?" {
		t.Errorf("CutPageContext = %q", got)
	}
	if got := hub.CutPageContext("plain\n[Talaria page chat? no"); got != "plain\n[Talaria page chat? no" {
		t.Errorf("CutPageContext of a plain message = %q", got)
	}

	one, err := hub.WithPageContext("q", hub.PageContext{Scope: scopeTab, Tabs: p.Tabs[:1]})
	if err != nil || !strings.Contains(one, "Only the active tab:\n- tab_id 12") || strings.Contains(one, "Text the user selected") {
		t.Errorf("tab scope = %q, %v", one, err)
	}
	none, err := hub.WithPageContext("q", hub.PageContext{Scope: scopeWindow})
	if err != nil || !strings.Contains(none, "No tab can be read") {
		t.Errorf("no tabs = %q, %v", none, err)
	}
	for name, bad := range map[string]hub.PageContext{
		"scope":     {Scope: "all"},
		"tab id":    {Scope: scopeTab, Tabs: []hub.PageTab{{ID: 0, URL: exampleURL}}},
		"twice":     {Scope: scopeWindow, Tabs: []hub.PageTab{{ID: 1, URL: exampleURL}, {ID: 1, URL: exampleURL}}},
		"scheme":    {Scope: scopeTab, Tabs: []hub.PageTab{{ID: 1, URL: "file:///etc/passwd"}}},
		"selection": {Scope: scopeWindow, Selection: &hub.PageSelection{Text: "x"}},
		"too many":  {Scope: scopeWindow, Tabs: make([]hub.PageTab, hub.MaxPageTabs+1)},
	} {
		if _, err := hub.WithPageContext("q", bad); !errors.Is(err, hub.ErrPageContext) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
