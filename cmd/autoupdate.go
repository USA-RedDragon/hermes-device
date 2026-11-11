package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/attest"
	"github.com/USA-RedDragon/hermes-device/internal/autoupdate"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/setup"
	"github.com/spf13/cobra"
)

// exitRestart is run's exit status after an auto-update: EX_TEMPFAIL, so
// that the service manager starts the new binary also when it restarts
// only after a failure.
const exitRestart = 75

// restartError is runDaemon's result when it stopped to run a new release.
type restartError struct{ tag string }

func (e *restartError) Error() string { return "stopped to restart into " + e.tag }

// daemonLookupMaxAge is how old a cached release lookup the daemon reuses,
// so that a daemon that restarts often does not use up GitHub's
// unauthenticated limit, which also counts 304 answers.
const daemonLookupMaxAge = time.Hour

// NoNoticeEnv, when set to anything, turns the update notice of
// interactive commands off, for example in scripts.
const NoNoticeEnv = config.EnvPrefix + "NO_UPDATE_NOTICE"

// defaultReleaseLookup is the GitHub release lookup with the shared cache.
func defaultReleaseLookup(userAgent string) *autoupdate.Releases {
	return &autoupdate.Releases{
		APIURL: autoupdate.DefaultAPIURL, Owner: attest.Owner, Repo: attest.Repo,
		Client: &http.Client{Timeout: 30 * time.Second}, UserAgent: userAgent, Path: autoupdate.DefaultCachePath(),
	}
}

// interactive marks c as a command a person runs: it tells them about a
// newer release, without waiting for the network to start.
func (b *base) interactive(c *cobra.Command) *cobra.Command {
	run := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		b.noticeOut = cmd.ErrOrStderr()
		b.noticeCtx = cmd.Context()
		err := run(cmd, args)
		if b.notice != nil {
			b.notice.Finish()
		}
		return err
	}
	return c
}

// startNotice starts the update notice of an interactive command once its
// config is loaded.
func (b *base) startNotice(cfg *config.Config) {
	if b.noticeOut == nil || b.notice != nil || b.getenv(NoNoticeEnv) != "" {
		return
	}
	ctx := b.noticeCtx
	if ctx == nil {
		ctx = context.Background()
	}
	b.notice = &autoupdate.Notice{Releases: b.releaseLookup(b.userAgent()), Version: b.version, Auto: cfg.Update.Auto, Out: b.noticeOut}
	b.notice.Start(ctx)
}

// underService reports whether the talaria service runs this
// process, so that it starts again after an exit.
func underService() bool {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/self/cgroup")
		return err == nil && (strings.Contains(string(data), "/"+setup.UnitName) || strings.Contains(string(data), "/"+setup.LegacyUnitName))
	case "darwin":
		name := os.Getenv("XPC_SERVICE_NAME")
		return name == setup.LaunchdLabel || name == setup.LegacyLaunchdLabel
	}
	return false
}

// errNotInstalled is why the daemon does not update a binary that setup
// did not install.
var errNotInstalled = errors.New("this daemon does not run the binary setup installed")

// autoTarget is the binary the daemon updates: the one setup installed,
// and only when the daemon runs it.
func (b *base) autoTarget(home string) (string, error) {
	installed := setup.InstalledPath(home)
	exe, err := b.executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	if installed, err = filepath.EvalSymlinks(installed); err != nil {
		return "", fmt.Errorf("%w (%s): %w", errNotInstalled, exe, err)
	}
	if exe != installed {
		return "", fmt.Errorf("%w (%s, not %s)", errNotInstalled, exe, installed)
	}
	return installed, nil
}

// daemonUpdater returns the auto-updater of the run daemon, or nil when
// auto-update is off here.
func (b *base) daemonUpdater(cfg *config.Config) *autoupdate.Daemon {
	off := func(why string, args ...any) *autoupdate.Daemon {
		slog.Info("Auto-update is off: "+why, args...)
		return nil
	}
	switch {
	case !cfg.Update.Auto:
		return off("update.auto is false")
	case cfg.Hub.Token != "" || cfg.Hub.TokenFile != "":
		return off("this is a service device (hub.token or hub.token-file); its image is updated elsewhere")
	case !autoupdate.Stable("v" + strings.TrimPrefix(b.version, "v")):
		return off("this is a development build", "version", b.version)
	case !b.underService():
		return off("the talaria service does not run this daemon; run talaria update to update")
	}
	sys, err := b.system()
	if err != nil {
		return off("no home directory", "error", err)
	}
	target, err := b.autoTarget(sys.Home)
	if err != nil {
		return off("run talaria update to update", "error", err)
	}
	rel := b.releaseLookup(b.userAgent())
	slog.Info("Auto-update is on", "interval", cfg.Update.Interval, "binary", target)
	return &autoupdate.Daemon{
		Version:   b.version,
		Interval:  cfg.Update.Interval,
		Latest:    func(ctx context.Context) (string, error) { return rel.Latest(ctx, daemonLookupMaxAge) },
		Installed: func(ctx context.Context) (string, error) { return sys.InstalledVersion(ctx, target) },
		Install:   func(ctx context.Context, tag string) error { return b.autoInstall(ctx, sys, target, tag) },
	}
}

// autoInstall installs release tag at target through the same steps as
// talaria update: checksum, attestation and a run of the new
// binary. It never skips a check, never installs an older or the same
// release, and restarts nothing.
func (b *base) autoInstall(ctx context.Context, sys *setup.System, target, tag string) error {
	if !autoupdate.Stable(tag) {
		return fmt.Errorf("%q is not a stable release", tag)
	}
	disk, err := sys.InstalledVersion(ctx, target)
	if err != nil {
		return fmt.Errorf("read the version of %s: %w", target, err)
	}
	quiet := *sys
	quiet.Out = io.Discard
	o := &setup.UpdateOptions{
		Target: target, Version: disk, Tag: tag, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOARM: goarm(),
		Force: false, Restart: false, Check: false, SkipAttestation: false,
		Releases: b.releases(), Attestations: b.verifier(),
		Preflight: append(runArgs(append([]string{""}, b.runArgs...)), "--"+CheckConfigFlag),
	}
	o.Releases.UserAgent = b.userAgent()
	_, err = quiet.Update(ctx, o)
	return err
}
