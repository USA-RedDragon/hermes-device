package autoupdate

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Defaults of Daemon.
const (
	DefaultFirstMin = time.Minute
	DefaultFirstMax = 5 * time.Minute
	DefaultIdleWait = 30 * time.Minute
	DefaultIdlePoll = 10 * time.Second
	// Jitter is the most a check moves from the interval, as a fraction.
	Jitter = 0.1
)

// Daemon checks for a new release on a schedule and installs it, then
// returns so that the daemon can exit and its service manager can start
// the new binary.
type Daemon struct {
	// Version is the running binary's version.
	Version string
	// Interval is the time between checks; each check moves by up to
	// Jitter of it. The first check comes between FirstMin and FirstMax
	// after Run starts.
	Interval           time.Duration
	FirstMin, FirstMax time.Duration
	// Latest returns the latest stable release tag.
	Latest func(ctx context.Context) (string, error)
	// Installed returns the version of the binary on disk, which an
	// install that waits for a restart has already replaced.
	Installed func(ctx context.Context) (string, error)
	// Install downloads, verifies and installs release tag. It must not
	// skip a check.
	Install func(ctx context.Context, tag string) error
	// Drain reports whether no work runs on the device and, if so, stops
	// new work from starting. Run polls it every IdlePoll for up to
	// IdleWait.
	Drain              func() bool
	IdleWait, IdlePoll time.Duration
	// Rand returns a number in [0, 1); nil is math/rand/v2.
	Rand func() float64
	// Sleep waits d or until ctx ends; nil uses a timer.
	Sleep func(ctx context.Context, d time.Duration) bool
	Log   *slog.Logger
}

func (d *Daemon) rand() float64 {
	if d.Rand != nil {
		return d.Rand()
	}
	return rand.Float64() //nolint:gosec // jitter, not a secret
}

func (d *Daemon) sleep(ctx context.Context, dur time.Duration) bool {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (d *Daemon) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// FirstDelay is the wait before the first check.
func (d *Daemon) FirstDelay() time.Duration {
	lo, hi := d.FirstMin, d.FirstMax
	if lo <= 0 {
		lo = DefaultFirstMin
	}
	if hi < lo {
		hi = max(lo, DefaultFirstMax)
	}
	return lo + time.Duration(d.rand()*float64(hi-lo))
}

// NextDelay is the wait between checks: Interval, moved by up to Jitter
// either way.
func (d *Daemon) NextDelay() time.Duration {
	return time.Duration(float64(d.Interval) * (1 - Jitter + 2*Jitter*d.rand()))
}

// Run checks until ctx ends, and returns "" then. When it installed a
// release and the device was idle, it returns that release's tag; the
// device then refuses new work, and the caller must exit.
func (d *Daemon) Run(ctx context.Context) string {
	if !d.sleep(ctx, d.FirstDelay()) {
		return ""
	}
	for {
		if tag := d.Check(ctx); tag != "" {
			return tag
		}
		if !d.sleep(ctx, d.NextDelay()) {
			return ""
		}
	}
}

// Check runs one check. It returns the tag to restart into, or "". A
// failure is logged and waits for the next check, so nothing retries in
// a loop.
func (d *Daemon) Check(ctx context.Context) string {
	log := d.log()
	latest, err := d.Latest(ctx)
	if err != nil {
		log.Warn("Could not look up the latest hermes-device release; trying again at the next check", "error", err)
		return ""
	}
	if !Newer(latest, d.Version) {
		log.Debug("hermes-device is up to date", "version", d.Version, "latest", latest)
		return ""
	}
	disk, err := d.Installed(ctx)
	if err != nil {
		log.Warn("Could not read the version of the installed hermes-device", "error", err)
		return ""
	}
	switch {
	case Newer(latest, disk):
		log.Debug("Installing a new hermes-device release", "version", d.Version, "latest", latest)
		if err := d.Install(ctx, latest); err != nil {
			if ctx.Err() == nil {
				log.Warn("Did not install the new hermes-device release; trying again at the next check", "version", latest, "error", err)
			}
			return ""
		}
		log.Info("Installed a new hermes-device release", "old", d.Version, "new", latest)
	case "v"+trimV(disk) == latest:
		log.Debug("The new hermes-device release is installed and waits for a restart", "version", latest)
	default:
		log.Debug("The installed hermes-device is newer than the latest release; leaving it", "installed", disk, "latest", latest)
		return ""
	}
	if !d.waitIdle(ctx) {
		if ctx.Err() == nil {
			log.Info("Work is running; restarting into the new hermes-device release at a later check", "new", latest)
		}
		return ""
	}
	log.Info("Restarting into the new hermes-device release", "old", d.Version, "new", latest)
	return latest
}

func trimV(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		return v[1:]
	}
	return v
}

// waitIdle polls Drain until it succeeds or IdleWait passes.
func (d *Daemon) waitIdle(ctx context.Context) bool {
	wait, poll := d.IdleWait, d.IdlePoll
	if wait <= 0 {
		wait = DefaultIdleWait
	}
	if poll <= 0 {
		poll = DefaultIdlePoll
	}
	for waited := time.Duration(0); ; waited += poll {
		if d.Drain() {
			return true
		}
		if waited >= wait || !d.sleep(ctx, poll) {
			return false
		}
	}
}
