package plugininstall_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the repository root, two levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func pluginVersion(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "plugin", "device", "core.py"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^PLUGIN_VERSION = "([^"]+)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("core.py has no PLUGIN_VERSION")
	}
	return string(m[1])
}

func check(t *testing.T, root, tag string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "scripts/check-plugin-version.sh", tag)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestReleaseChecksPluginVersion: the check the Release workflow runs before it
// builds passes for the tag of the plugin's own version and fails for any other,
// so a release cannot ship a plugin that reports an older version (v0.25.3
// reported 0.25.2).
func TestReleaseChecksPluginVersion(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	v := pluginVersion(t, root)
	if out, err := check(t, root, "v"+v); err != nil {
		t.Fatalf("tag v%s: %v\n%s", v, err, out)
	}
	for _, tag := range []string{"v0.0.1", "v" + v + "1", "v99.0.0"} {
		out, err := check(t, root, tag)
		if err == nil || !strings.Contains(out, "plugin: report version") {
			t.Errorf("tag %s passed or gave no hint: %v\n%s", tag, err, out)
		}
	}
	if _, err := check(t, root, "v99.0.0-rc.1"); err != nil {
		t.Errorf("a pre-release tag is not checked: %v", err)
	}
}

// TestManifestVersionMatchesCore: plugin.yaml and core.py name one version.
func TestManifestVersionMatchesCore(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "plugin", "device", "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "\nversion: " + pluginVersion(t, root) + "\n"; !strings.Contains("\n"+string(b), want) {
		t.Fatalf("plugin.yaml does not have %q", strings.TrimSpace(want))
	}
}

// TestReleaseWorkflowRunsTheCheck: the check runs in the Release workflow.
func TestReleaseWorkflowRunsTheCheck(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `scripts/check-plugin-version.sh "$GITHUB_REF_NAME"`) {
		t.Fatal("release.yaml does not run scripts/check-plugin-version.sh before the build")
	}
}
