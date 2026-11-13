package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/USA-RedDragon/talaria/extension/firefox"
	"github.com/USA-RedDragon/talaria/internal/browser/headless"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/tokenstore"
)

// headlessRun is a headless Firefox that the daemon started.
type headlessRun struct {
	*headless.Supervisor
	done chan struct{}
}

// wait blocks until Firefox has stopped, after the daemon's context ended.
func (h headlessRun) wait() { <-h.done }

// headlessProfile is the directory of the headless Firefox's profile.
func headlessProfile(cfg *config.Config) (string, error) {
	if cfg.Browser.ProfileDir != "" {
		return cfg.Browser.ProfileDir, nil
	}
	dir := cfg.Store.StateDir
	if dir == "" {
		d, err := tokenstore.DefaultStateDir()
		if err != nil {
			return "", fmt.Errorf("find the state directory: %w", err)
		}
		dir = d
	}
	return filepath.Join(dir, "firefox-profile"), nil
}

// startHeadless registers this binary as the extension's native host, writes
// the profile and starts the supervised headless Firefox (docs/browser.md
// section 5.2). env is the commands' environment, which has no device token,
// so neither Firefox nor the pages it loads can read it.
func startHeadless(ctx context.Context, cfg *config.Config, version string, env []string) (headlessRun, error) {
	profile, err := headlessProfile(cfg)
	if err != nil {
		return headlessRun{}, err
	}
	sock, err := browserSocket(cfg)
	if err != nil {
		return headlessRun{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return headlessRun{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return headlessRun{}, err
	}
	if _, err := installHostManifest(exe, home, sock); err != nil {
		return headlessRun{}, fmt.Errorf("register the native messaging host: %w", err)
	}
	ver := ""
	if firefox.ValidVersion(version) {
		ver = version
	}
	o := headless.Options{Command: cfg.Browser.FirefoxCommand, ProfileDir: profile, Version: ver, Env: env,
		LogPath: filepath.Join(filepath.Dir(profile), "firefox.log")}
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		return headlessRun{}, err
	}
	if err := headless.Prepare(o); err != nil {
		return headlessRun{}, err
	}
	sup := headless.New(o)
	h := headlessRun{Supervisor: sup, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		sup.Run(ctx)
	}()
	slog.Info("Headless Firefox on", "profile", profile, "command", cfg.Browser.FirefoxCommand)
	return h, nil
}
