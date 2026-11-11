package autoupdate_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/autoupdate"
)

// fakeDaemon records what a Daemon does, with a clock that only moves when
// it sleeps.
type fakeDaemon struct {
	mu       sync.Mutex
	latest   string
	err      error
	disk     string
	install  error
	installs []string
	sleeps   []time.Duration
	// idleAfter is the number of Drain calls that find work running.
	idleAfter int
	drains    int
	logs      bytes.Buffer
}

func (f *fakeDaemon) daemon(version string) *autoupdate.Daemon {
	return &autoupdate.Daemon{
		Version:  version,
		Interval: 6 * time.Hour,
		Latest: func(context.Context) (string, error) {
			return f.latest, f.err
		},
		Installed: func(context.Context) (string, error) { return f.disk, nil },
		Install: func(_ context.Context, tag string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.installs = append(f.installs, tag)
			if f.install == nil {
				f.disk = strings.TrimPrefix(tag, "v")
			}
			return f.install
		},
		Drain: func() bool {
			f.drains++
			return f.drains > f.idleAfter
		},
		IdleWait: time.Minute, IdlePoll: 10 * time.Second,
		Rand: func() float64 { return 0.5 },
		Sleep: func(_ context.Context, d time.Duration) bool {
			f.sleeps = append(f.sleeps, d)
			return true
		},
		Log: slog.New(slog.NewTextHandler(&f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func TestScheduleAndJitter(t *testing.T) {
	t.Parallel()
	d := (&fakeDaemon{}).daemon(verNow)
	for r, want := range map[float64]time.Duration{0: 324 * time.Minute, 0.5: 6 * time.Hour, 0.999999: 396 * time.Minute} {
		d.Rand = func() float64 { return r }
		if got := d.NextDelay(); got.Round(time.Minute) != want {
			t.Errorf("NextDelay at %v = %s, want %s", r, got, want)
		}
		if got := d.FirstDelay(); got < autoupdate.DefaultFirstMin || got > autoupdate.DefaultFirstMax {
			t.Errorf("FirstDelay at %v = %s", r, got)
		}
	}
	d.Rand = nil
	seen := map[time.Duration]bool{}
	for range 20 {
		got := d.NextDelay()
		if got < 324*time.Minute || got > 396*time.Minute {
			t.Fatalf("NextDelay = %s, outside 6h +-10%%", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Error("NextDelay has no jitter")
	}
}

func TestRunChecksFirstSoonThenOnInterval(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{latest: tagNow, disk: verNow}
	d := f.daemon(verNow)
	ctx, cancel := context.WithCancel(t.Context())
	checks := 0
	d.Latest = func(context.Context) (string, error) {
		checks++
		if checks == 3 {
			cancel()
		}
		return tagNow, nil
	}
	d.Sleep = func(ctx context.Context, dur time.Duration) bool {
		f.sleeps = append(f.sleeps, dur)
		return ctx.Err() == nil
	}
	if tag := d.Run(ctx); tag != "" {
		t.Fatalf("Run = %q", tag)
	}
	want := []time.Duration{3 * time.Minute, 6 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	if len(f.sleeps) != len(want) {
		t.Fatalf("sleeps %v, want %v", f.sleeps, want)
	}
	for i := range want {
		if f.sleeps[i] != want[i] {
			t.Errorf("sleep %d = %s, want %s", i, f.sleeps[i], want[i])
		}
	}
}

func TestCheckInstallsAndRestartsWhenIdle(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{latest: tagNext, disk: verNow, idleAfter: 2}
	if tag := f.daemon(verNow).Check(t.Context()); tag != tagNext {
		t.Fatalf("Check = %q; logs:\n%s", tag, f.logs.String())
	}
	if len(f.installs) != 1 || f.drains != 3 {
		t.Errorf("installs %v, drains %d", f.installs, f.drains)
	}
	if !strings.Contains(f.logs.String(), `level=INFO msg="Installed a new hermes-device release" old=0.14.0 new=v0.15.0`) {
		t.Errorf("no install log:\n%s", f.logs.String())
	}
}

func TestCheckDefersWhileBusy(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{latest: tagNext, disk: verNow, idleAfter: 1000}
	d := f.daemon(verNow)
	if tag := d.Check(t.Context()); tag != "" {
		t.Fatalf("Check = %q while busy", tag)
	}
	if f.drains != 7 {
		t.Errorf("Drain called %d times in a 1m wait with 10s polls, want 7", f.drains)
	}
	f.idleAfter = f.drains
	if tag := d.Check(t.Context()); tag != tagNext {
		t.Fatalf("next Check = %q", tag)
	}
	if len(f.installs) != 1 {
		t.Errorf("installed %v; the second check must reuse the installed binary", f.installs)
	}
}

func TestCheckNeverDowngradesOrInstallsPrerelease(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]*fakeDaemon{
		"same":        {latest: tagNow, disk: verNow},
		"older":       {latest: "v0.13.0", disk: verNow},
		"prerelease":  {latest: "v0.15.0-rc.1", disk: verNow},
		"build":       {latest: "v0.15.0+x", disk: verNow},
		"newer disk":  {latest: tagNext, disk: "0.16.0"},
		"lookup fail": {err: errors.New("offline"), disk: verNow},
	} {
		if tag := f.daemon(verNow).Check(t.Context()); tag != "" || len(f.installs) != 0 || f.drains != 0 {
			t.Errorf("%s: Check = %q, installs %v, drains %d", name, tag, f.installs, f.drains)
		}
	}
	f := &fakeDaemon{latest: tagNext, disk: "dev"}
	if tag := f.daemon("dev").Check(t.Context()); tag != "" || len(f.installs) != 0 {
		t.Errorf("dev build: Check = %q, installs %v", tag, f.installs)
	}
}

func TestCheckVerificationFailureBlocksTheRestart(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{latest: tagNext, disk: verNow, install: errors.New("the release archive's attestation did not verify")}
	if tag := f.daemon(verNow).Check(t.Context()); tag != "" {
		t.Fatalf("Check = %q after a failed install", tag)
	}
	if f.drains != 0 || f.disk != verNow {
		t.Errorf("drains %d, disk %s", f.drains, f.disk)
	}
	if !strings.Contains(f.logs.String(), "level=WARN") || !strings.Contains(f.logs.String(), "attestation did not verify") {
		t.Errorf("no warning:\n%s", f.logs.String())
	}
}

func TestFailuresWaitForTheNextCheck(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{latest: tagNext, disk: verNow, install: errors.New("verification failed")}
	d := f.daemon(verNow)
	ctx, cancel := context.WithCancel(t.Context())
	d.Sleep = func(ctx context.Context, dur time.Duration) bool {
		f.sleeps = append(f.sleeps, dur)
		if len(f.sleeps) == 4 {
			cancel()
		}
		return ctx.Err() == nil
	}
	d.Run(ctx)
	if len(f.installs) != 3 {
		t.Errorf("%d installs in 3 checks", len(f.installs))
	}
	for _, s := range f.sleeps[1:] {
		if s < 5*time.Hour {
			t.Errorf("retried after %s", s)
		}
	}
}
