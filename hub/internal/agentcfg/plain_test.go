package agentcfg

import (
	"regexp"
	"testing"
)

func TestHumanize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"web_search":             "Web search",
		"systematic-debugging":   "Systematic debugging",
		"mcp_servers":            "MCP servers",
		"agent.locked_keys":      "Agent locked keys",
		"github":                 "GitHub",
		"Already Fine":           "Already Fine",
		"":                       "",
		"home_assistant_bridge2": "Home assistant bridge2",
	} {
		if got := Humanize(in); got != want {
			t.Errorf("Humanize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlainToolset(t *testing.T) {
	t.Parallel()
	got := plainToolset(Toolset{Name: "web", Label: "Web Search & Scraping", Description: "web_search, web_extract"})
	if got.Label != "Web search" || got.Description != "Search the web and read pages" {
		t.Errorf("web: %+v", got)
	}
	// Unknown ids: humanized label, and an upstream description with ids is dropped.
	got = plainToolset(Toolset{Name: "new_upstream_set", Label: "", Description: "calls new_tool and other_tool"})
	if got.Label != "New upstream set" || got.Description != "" {
		t.Errorf("unknown: %+v", got)
	}
	got = plainToolset(Toolset{Name: "third_party", Label: "Third party", Description: "Does a plain thing"})
	if got.Label != "Third party" || got.Description != "Does a plain thing" {
		t.Errorf("plain: %+v", got)
	}
	// Every label and description of the table is free of ids.
	id := regexp.MustCompile(`[a-z]+_[a-z_]+`)
	for k, p := range plainToolsets {
		if id.MatchString(p[0]) || id.MatchString(p[1]) || p[0] == "" || p[1] == "" {
			t.Errorf("%s: %q %q", k, p[0], p[1])
		}
	}
}
