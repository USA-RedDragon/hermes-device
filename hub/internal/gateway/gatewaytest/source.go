package gatewaytest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
)

// HermesSource returns the Hermes checkout named by HERMES_AGENT_SRC (default
// ~/.hermes/hermes-agent) for the tests that check Hermes source lines. The
// checkout must contain marker and be at HERMES_AGENT_VERSION, which defaults
// to gateway.HermesVersion; set it to check a newer Hermes before an upgrade.
// Without HERMES_AGENT_SRC, a missing checkout skips the test. With it, a
// checkout that lacks marker fails, so a Hermes release that moves the file
// cannot pass by skipping.
func HermesSource(t *testing.T, marker string) string {
	t.Helper()
	src, explicit := os.LookupEnv("HERMES_AGENT_SRC")
	if src == "" {
		explicit = false
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".hermes", "hermes-agent")
	}
	if _, err := os.Stat(filepath.Join(src, marker)); err != nil { //nolint:gosec // a test reads the checkout it is told to
		if explicit {
			t.Fatalf("Hermes checkout at %s has no %s: %v", src, marker, err)
		}
		t.Skipf("no Hermes checkout at %s", src)
	}
	want := os.Getenv("HERMES_AGENT_VERSION")
	if want == "" {
		want = gateway.HermesVersion
	}
	if v := sourceVersion(t, src); v != want {
		t.Fatalf("Hermes checkout at %s is %q, want %s (gateway.HermesVersion is %s)", src, v, want, gateway.HermesVersion)
	}
	return src
}

func sourceVersion(t *testing.T, src string) string {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(src, "install-stamp.json")); err == nil { //nolint:gosec // a test reads the checkout it is told to
		var stamp struct {
			BaseVersion string `json:"baseVersion"`
			Distance    int    `json:"distance"`
			Dirty       bool   `json:"dirty"`
		}
		if json.Unmarshal(b, &stamp) == nil && stamp.BaseVersion != "" && stamp.Distance == 0 && !stamp.Dirty {
			return "v" + stamp.BaseVersion
		}
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", src, "describe", "--tags", "--exact-match").Output() //nolint:gosec // a test runs git on the checkout it is told to
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
