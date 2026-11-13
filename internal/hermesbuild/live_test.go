package hermesbuild_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/hermesbuild"
)

// TestLive pulls a real image and runs its hermes --version. It runs only
// with HERMESBUILD_LIVE_IMAGE (a reference pinned by digest) and
// HERMESBUILD_LIVE_DIR (the store directory) set.
func TestLive(t *testing.T) {
	t.Parallel()
	image, dir := os.Getenv("HERMESBUILD_LIVE_IMAGE"), os.Getenv("HERMESBUILD_LIVE_DIR")
	if image == "" || dir == "" {
		t.Skip("HERMESBUILD_LIVE_IMAGE and HERMESBUILD_LIVE_DIR are unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	s := &hermesbuild.Store{Dir: dir, Progress: func(done, total int64) { t.Logf("%d of %d bytes", done, total) }}
	start := time.Now()
	b, err := s.Ensure(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("build %s from %s in %s", b.Dir, b.Meta.Source, time.Since(start))
	launcher, err := b.Launcher()
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.Env(b, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, launcher, "--version") //nolint:gosec // the build this test just pulled and verified
	cmd.Env = env
	cmd.Dir = s.Home()
	out, err := cmd.CombinedOutput()
	t.Logf("%s --version:\n%s", launcher, out)
	if err != nil {
		t.Fatal(err)
	}
}
