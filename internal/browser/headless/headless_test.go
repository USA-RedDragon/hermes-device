package headless_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// fakeFirefox appends a line to log at each start and exits, or sleeps when
// the log has two lines.
func fakeFirefox(t *testing.T, dir, log string) string {
	t.Helper()
	p := filepath.Join(dir, "fakefox")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nif [ $(wc -l < " + log + ") -ge 2 ]; then exec sleep 60; fi\nexit 1\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return p
}

func TestSupervisorRestartsFirefoxAndStopsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "starts")
	profile := filepath.Join(dir, "profile")
	sup := headless.New(headless.Options{Command: fakeFirefox(t, dir, log), ProfileDir: profile, Env: os.Environ()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	// The first start exits at once; Launch skips the backoff.
	deadline := time.Now().Add(10 * time.Second)
	for !sup.Running() {
		if time.Now().After(deadline) {
			t.Fatal("Firefox was not started again after it exited")
		}
		_ = sup.Launch(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--headless --no-remote --profile "+profile) {
		t.Fatalf("command line = %q", data)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor did not stop Firefox")
	}
	if sup.Running() {
		t.Fatal("Running after stop")
	}
}

func TestLaunchFailsWithoutCommand(t *testing.T) {
	t.Parallel()
	sup := headless.New(headless.Options{Command: "no-such-firefox-binary", ProfileDir: t.TempDir()})
	if err := sup.Launch(context.Background()); err == nil {
		t.Fatal("Launch accepted a missing command")
	}
}
