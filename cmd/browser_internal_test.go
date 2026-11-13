package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	firefox "github.com/USA-RedDragon/talaria/extension/firefox"
	"github.com/USA-RedDragon/talaria/internal/browser/launch"
)

// extArgs is "browser extension --out" followed by rest.
func extArgs(rest ...string) []string {
	return append([]string{"browser", "extension", "--out"}, rest...)
}

func extensionVersion(t *testing.T, binVersion string, args ...string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ext")
	root := NewCommand(binVersion, "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(extArgs(append([]string{dir}, args...)...))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "background.js")); err != nil {
		t.Fatal(err)
	}
	return m.Version, out.String()
}

func TestBrowserExtensionVersion(t *testing.T) {
	t.Parallel()
	embedded, err := firefox.Manifest("")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(embedded, &m); err != nil {
		t.Fatal(err)
	}
	const release = "0.9.1"
	cases := []struct {
		bin  string
		args []string
		want string
	}{
		{"dev", nil, m.Version},
		{"0.9.1-rc.1", nil, m.Version},
		{release, nil, release},
		{release, []string{"--ext-version", "1.2.3"}, "1.2.3"},
	}
	for _, c := range cases {
		got, out := extensionVersion(t, c.bin, c.args...)
		if got != c.want {
			t.Errorf("binary %s %v: version %q, want %q", c.bin, c.args, got, c.want)
		}
		if !strings.Contains(out, firefox.ReleasesURL) {
			t.Errorf("output does not point at the releases: %q", out)
		}
	}
}

func TestBrowserExtensionBadVersion(t *testing.T) {
	t.Parallel()
	root := NewCommand("dev", "test")
	root.SetOut(&bytes.Buffer{})
	root.SetArgs(extArgs(t.TempDir(), "--ext-version", "v1.0"))
	if err := root.Execute(); err == nil {
		t.Fatal("want an error for an invalid version")
	}
}

func TestWithLock(t *testing.T) {
	t.Parallel()
	got := string(withLock([]byte(`{"tab_id":3}`), launch.Lock{Known: true, Locked: true, Source: "logind"}))
	if got != `{"screen_lock":{"known":true,"locked":true,"source":"logind"},"tab_id":3}` {
		t.Errorf("withLock = %s", got)
	}
	if got := string(withLock([]byte(`[1]`), launch.Lock{})); got != `[1]` {
		t.Errorf("withLock of a non-object = %s", got)
	}
}
