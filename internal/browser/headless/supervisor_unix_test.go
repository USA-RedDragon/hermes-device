//go:build unix

package headless_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/browser/headless"
)

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
