package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	firefox "github.com/USA-RedDragon/hermes-device/extension/firefox"
)

func extensionVersion(t *testing.T, binVersion string, args ...string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ext")
	root := NewCommand(binVersion, "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(append([]string{"browser", "extension", "--out", dir}, args...))
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
	root.SetArgs([]string{"browser", "extension", "--out", t.TempDir(), "--ext-version", "v1.0"})
	if err := root.Execute(); err == nil {
		t.Fatal("want an error for an invalid version")
	}
}
