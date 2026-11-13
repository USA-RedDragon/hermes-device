//go:build unix

package claudeusage

import "testing"

func TestDefaultSources(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	got := DefaultSources(home, func(string) string { return "" }, []string{"/srv/claude-b", home + "/.claude/"})
	if len(got) != 2 || got[0].Dir != home+"/.claude" || got[0].ConfigFile != home+"/.claude.json" || got[1].Dir != "/srv/claude-b" {
		t.Fatalf("sources %+v", got)
	}
	got = DefaultSources(home, func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "/a, relative,/b"
		}
		return ""
	}, nil)
	if len(got) != 2 || got[0].Dir != "/a" || got[1].Dir != "/b" {
		t.Fatalf("sources %+v", got)
	}
}
