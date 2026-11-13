package headless_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/browser/headless"
	"github.com/USA-RedDragon/talaria/internal/browser/host"
)

func TestPrepareWritesProfile(t *testing.T) {
	t.Parallel()
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"lock", ".parentlock", "cookies.sqlite"} {
		if err := os.WriteFile(filepath.Join(profile, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(profile, "extensions", host.ExtensionID, "dropped.js")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := headless.Prepare(headless.Options{ProfileDir: profile, Version: "1.2.3"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(profile, "extensions", host.ExtensionID, "manifest.json"))
	if err != nil || !strings.Contains(string(manifest), `"version": "1.2.3"`) {
		t.Fatalf("manifest = %q, %v", manifest, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a file of the old extension stayed: %v", err)
	}
	js, err := os.ReadFile(filepath.Join(profile, "user.js"))
	if err != nil || !strings.Contains(string(js), `"xpinstall.signatures.required", false`) {
		t.Fatalf("user.js = %q, %v", js, err)
	}
	for _, f := range []string{"lock", ".parentlock"} {
		if _, err := os.Stat(filepath.Join(profile, f)); !os.IsNotExist(err) {
			t.Fatalf("%s stayed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(profile, "cookies.sqlite")); err != nil {
		t.Fatalf("the profile's data was removed: %v", err)
	}
}

func TestPrepareNeedsAbsoluteProfile(t *testing.T) {
	t.Parallel()
	if err := headless.Prepare(headless.Options{ProfileDir: "relative"}); err == nil {
		t.Fatal("a relative profile directory was accepted")
	}
}

func TestLaunchFailsWithoutCommand(t *testing.T) {
	t.Parallel()
	sup := headless.New(headless.Options{Command: "no-such-firefox-binary", ProfileDir: t.TempDir()})
	if err := sup.Launch(context.Background()); err == nil {
		t.Fatal("Launch accepted a missing command")
	}
}
