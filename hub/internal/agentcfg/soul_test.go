package agentcfg_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
)

// The setup step of the compose kit and the chart writes the same default
// personality as the hub's Reset, and neither names the agent's maker.
func TestDefaultSoulMatchesSetupStep(t *testing.T) {
	t.Parallel()
	for _, kit := range []string{"compose", filepath.Join("charts", "talaria")} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", kit, "files", "hermes-init.py"))
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`(?s)TALARIA_SOUL = """\\\n(.*?)"""`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no TALARIA_SOUL", kit)
		}
		if got := strings.ReplaceAll(string(m[1]), "\\\n", ""); got != agentcfg.DefaultSoul {
			t.Fatalf("%s: TALARIA_SOUL differs from DefaultSoul:\n%q\n%q", kit, got, agentcfg.DefaultSoul)
		}
	}
	for _, bad := range []string{"hermes", "nous", "claude", "anthropic", "openai"} {
		if strings.Contains(strings.ToLower(agentcfg.DefaultSoul), bad) {
			t.Fatalf("default personality names %q", bad)
		}
	}
	if strings.ContainsAny(agentcfg.DefaultSoul, "\u2014\u2013") {
		t.Fatal("default personality has a dash")
	}
}
