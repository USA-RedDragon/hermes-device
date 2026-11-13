// Package headless runs a Firefox of the daemon's own, without a display, for
// a device that has no desktop: the agent sandbox (docs/browser.md section
// 5.2). The daemon prepares a profile with the Talaria extension installed in
// it, starts Firefox on that profile and starts it again when it exits.
package headless

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/extension/firefox"
	"github.com/USA-RedDragon/talaria/internal/browser/host"
)

// Backoff bounds how soon Firefox starts again after it exited.
const (
	minBackoff = 2 * time.Second
	maxBackoff = time.Minute
	// stableRun is how long Firefox must have run for the next start not to
	// count as a crash loop.
	stableRun = time.Minute
	// stopGrace is how long Firefox gets to close after SIGTERM.
	stopGrace = 10 * time.Second
)

// userJS holds the preferences of the profile. Release and ESR builds of
// Firefox from a distribution (Alpine's firefox-esr) accept an add-on that
// Mozilla did not sign when xpinstall.signatures.required is false; the
// extension is the one embedded in this binary, so it is the one the daemon
// speaks to. Nothing updates by itself: the image carries the version.
const userJS = `user_pref("xpinstall.signatures.required", false);
user_pref("extensions.autoDisableScopes", 0);
user_pref("extensions.update.enabled", false);
user_pref("extensions.update.autoUpdateDefault", false);
user_pref("extensions.getAddons.cache.enabled", false);
user_pref("app.update.auto", false);
user_pref("app.update.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("browser.startup.page", 0);
user_pref("browser.newtabpage.enabled", false);
user_pref("browser.tabs.warnOnClose", false);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("toolkit.telemetry.enabled", false);
user_pref("toolkit.telemetry.unified", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("network.captive-portal-service.enabled", false);
user_pref("network.connectivity-service.enabled", false);
user_pref("browser.safebrowsing.malware.enabled", false);
user_pref("browser.safebrowsing.phishing.enabled", false);
user_pref("signon.rememberSignons", true);
user_pref("dom.ipc.processCount", 2);
`

// Options configure a Supervisor.
type Options struct {
	// Command is the Firefox binary: a name on PATH or an absolute path.
	Command string
	// ProfileDir is the Firefox profile, on a disk that survives restarts.
	ProfileDir string
	// Version is the extension's manifest version ("" keeps the embedded one).
	Version string
	// Env is the environment Firefox starts with.
	Env []string
	// LogPath is where Firefox's output goes, rewritten at each start. Empty
	// discards it.
	LogPath string
}

// Prepare writes the profile's preferences and the extension, and removes the
// lock files that a Firefox which did not close leaves behind.
func Prepare(o Options) error {
	if o.ProfileDir == "" || !filepath.IsAbs(o.ProfileDir) {
		return fmt.Errorf("headless profile directory %q is not an absolute path", o.ProfileDir)
	}
	ext := filepath.Join(o.ProfileDir, "extensions", host.ExtensionID)
	// The extension directory is replaced whole, so a file that a newer
	// version dropped does not stay.
	if err := os.RemoveAll(ext); err != nil {
		return err
	}
	if err := os.MkdirAll(o.ProfileDir, 0o700); err != nil {
		return err
	}
	if err := firefox.Write(ext, o.Version); err != nil {
		return fmt.Errorf("write the extension: %w", err)
	}
	if err := os.WriteFile(filepath.Join(o.ProfileDir, "user.js"), []byte(userJS), 0o600); err != nil {
		return err
	}
	for _, name := range []string{"lock", ".parentlock"} {
		if err := os.Remove(filepath.Join(o.ProfileDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Supervisor keeps one headless Firefox running.
type Supervisor struct {
	o    Options
	wake chan struct{}

	mu      sync.Mutex
	running bool
}

// New returns a Supervisor. Run starts Firefox.
func New(o Options) *Supervisor {
	if o.Command == "" {
		o.Command = "firefox"
	}
	return &Supervisor{o: o, wake: make(chan struct{}, 1)}
}

// Running reports whether a Firefox process is up.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Launch is the device's StartBrowser: when Firefox is down it makes the
// supervisor start it now instead of after its backoff. It never starts a
// second Firefox.
func (s *Supervisor) Launch(context.Context) error {
	if _, err := exec.LookPath(s.o.Command); err != nil {
		return fmt.Errorf("browser.firefox-command %q: %w", s.o.Command, err)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// Run starts Firefox and starts it again whenever it exits, until ctx ends.
// It returns after Firefox has stopped.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		ran := time.Since(started)
		if ran >= stableRun {
			backoff = minBackoff
		}
		slog.Warn("Headless Firefox exited; starting it again", "after", ran.Round(time.Second), "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (s *Supervisor) runOnce(ctx context.Context) error {
	for _, name := range []string{"lock", ".parentlock"} {
		_ = os.Remove(filepath.Join(s.o.ProfileDir, name))
	}
	cmd := exec.Command(s.o.Command, "--headless", "--no-remote", "--profile", s.o.ProfileDir, "about:blank") //nolint:gosec,noctx // the operator's own config; Firefox is stopped by the supervisor, not by a context
	cmd.Env = s.o.Env
	cmd.SysProcAttr = group()
	if s.o.LogPath != "" {
		if f, err := os.OpenFile(s.o.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			defer f.Close()
			cmd.Stdout, cmd.Stderr = f, f
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.o.Command, err)
	}
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	slog.Info("Headless Firefox started", "pid", cmd.Process.Pid, "profile", s.o.ProfileDir)
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	select {
	case <-exited:
		return waitErr
	case <-ctx.Done():
		terminate(cmd, true)
		select {
		case <-exited:
		case <-time.After(stopGrace):
			terminate(cmd, false)
			<-exited
		}
		return ctx.Err()
	}
}
