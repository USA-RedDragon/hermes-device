package agentcfg_test

import (
	"regexp"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
)

const webSearch = "web_search"

func TestHumanize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		webSearch:                "Web search",
		"systematic-debugging":   "Systematic debugging",
		"mcp_servers":            "MCP servers",
		"agent.locked_keys":      "Agent locked keys",
		"github":                 "GitHub",
		"ascii-video":            "ASCII video",
		"Already Fine":           "Already Fine",
		"":                       "",
		"home_assistant_bridge2": "Home assistant bridge2",
	} {
		if got := agentcfg.Humanize(in); got != want {
			t.Errorf("Humanize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlainToolset(t *testing.T) {
	t.Parallel()
	got := agentcfg.PlainToolset(agentcfg.Toolset{Name: "web", Label: "Web Search & Scraping", Description: webSearch + ", web_extract"})
	if got.Label != "Web search" || got.Description != "Search the web and read pages" {
		t.Errorf("web: %+v", got)
	}
	// Unknown ids: humanized label, and an upstream description with ids is dropped.
	got = agentcfg.PlainToolset(agentcfg.Toolset{Name: "new_upstream_set", Label: "", Description: "calls new_tool and other_tool"})
	if got.Label != "New upstream set" || got.Description != "" {
		t.Errorf("unknown: %+v", got)
	}
	got = agentcfg.PlainToolset(agentcfg.Toolset{Name: "third_party", Label: "Third party", Description: "Does a plain thing"})
	if got.Label != "Third party" || got.Description != "Does a plain thing" {
		t.Errorf("plain: %+v", got)
	}
	// Every id the table names answers words, never an id.
	id := regexp.MustCompile(`[a-z]+_[a-z_]+`)
	for _, k := range []string{"web", "browser", "terminal", "file", "code_execution", "vision", "video", "image_gen", "video_gen", "x_search", "tts", "stt", "skills", "todo", "kanban", "memory", "context_engine", "session_search", "connections", "clarify", "delegation", "cronjob", "discord", "discord_admin", "computer_use", "spotify", "a2a", "homeassistant", "rl"} {
		p := agentcfg.PlainToolset(agentcfg.Toolset{Name: k, Description: webSearch})
		if id.MatchString(p.Label) || id.MatchString(p.Description) || p.Label == "" || p.Description == "" || p.Description == webSearch {
			t.Errorf("%s: %q %q", k, p.Label, p.Description)
		}
	}
}
