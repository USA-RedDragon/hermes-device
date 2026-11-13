package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/spf13/cobra"
)

// goosDarwin is macOS's GOOS.
const (
	goosDarwin = "darwin"
	goosLinux  = "linux"
)

// handOffWait is how long a process that handed over to the talaria
// service waits to be stopped before it exits by itself.
const handOffWait = 2 * time.Minute

// WithMigration makes every command that runs on a desktop first move
// the files of a release before the rename to the talaria names. main
// sets it; tests leave it off, so they never touch the real home.
func WithMigration() Option {
	return func(b *base) { b.migrate = true }
}

// underLegacyService reports whether the service of a release before the
// rename runs this process.
func underLegacyService() bool {
	switch runtime.GOOS {
	case goosLinux:
		data, err := os.ReadFile("/proc/self/cgroup")
		return err == nil && strings.Contains(string(data), "/"+setup.LegacyUnitName)
	case goosDarwin:
		return os.Getenv("XPC_SERVICE_NAME") == setup.LegacyLaunchdLabel
	}
	return false
}

// skipMigration lists the commands that must not move files: those that
// run in the Hermes pod, the auto-update check of a new release (the old
// daemon still runs), and those that only print.
func skipMigration(c *cobra.Command) bool {
	switch c.Name() {
	case "acp-host", "extension", "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	case installCommand:
		return c.Parent() != nil && c.Parent().Name() == "plugin"
	case runCommand:
		check, _ := c.Flags().GetBool(CheckConfigFlag)
		return check
	}
	return false
}

// installedSelf reports whether this process runs the installed binary,
// at its legacy or its talaria path. A copy run from elsewhere (a
// download, a build) does not move an installation: it would move an
// older binary into the talaria path while the legacy daemon still needs
// it.
func (b *base) installedSelf() bool {
	exe, err := b.executable()
	if err != nil {
		return false
	}
	fe, err := os.Stat(exe)
	if err != nil {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, p := range []string{
		filepath.Join(home, setup.LegacyBinDir, setup.LegacyBinaryName),
		filepath.Join(home, setup.BinDir, setup.BinaryName),
	} {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fe, fi) {
			return true
		}
	}
	return false
}

// followConfig points --config at the moved config file when it names the
// legacy one, which is gone, also in os.Args, from which run takes the
// arguments of the auto-update check of a new release.
func followConfig(c *cobra.Command, sys *setup.System) {
	f := c.Flags().Lookup("config")
	if f == nil {
		return
	}
	old := f.Value.String()
	moved := sys.MigratedConfigPath(old)
	if moved == old {
		return
	}
	if _, err := os.Stat(old); err == nil {
		return
	}
	if _, err := os.Stat(moved); err != nil {
		return
	}
	if err := c.Flags().Set("config", moved); err != nil {
		return
	}
	for i, a := range os.Args {
		switch a {
		case old:
			os.Args[i] = moved
		case "--config=" + old:
			os.Args[i] = "--config=" + moved
		case "-c=" + old:
			os.Args[i] = "-c=" + moved
		}
	}
	slog.Info("Reading the moved config file", "config", moved)
}

// migrateOnStart runs setup.Migrate before a command and logs what it did.
// A failure is logged and the command still runs. After a hand-over to
// the talaria service, the process exits instead.
func (b *base) migrateOnStart(c *cobra.Command) {
	if !b.migrate {
		return
	}
	sys, err := b.system()
	if err != nil {
		return
	}
	defer followConfig(c, sys)
	if skipMigration(c) {
		return
	}
	under := c.Name() == runCommand && underLegacyService()
	if !under && !sys.NeedsMigration() {
		return
	}
	if !under && c.Name() != "setup" && !b.installedSelf() {
		slog.Debug("Not moving the hermes-device installation: this binary is not the installed one")
		return
	}
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	keep := !under && sys.LegacyServiceActive(ctx)
	// The hand-over to the talaria service stops this process during the
	// migration; the signal must not end it before it logs.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigc)
	m, err := sys.Migrate(ctx, setup.MigrateOptions{UnderLegacyService: under, KeepService: keep})
	for _, mv := range m.Moved {
		slog.Info("Migrated from hermes-device to talaria", "moved", mv)
	}
	for _, n := range m.Notes {
		slog.Info("Migrated from hermes-device to talaria", "note", n)
	}
	if keep {
		slog.Info("The hermes-device service runs; it moves to the talaria service when it restarts")
	}
	if err != nil {
		slog.Warn("The migration from hermes-device to talaria is not complete; the next start tries again", "error", err)
	}
	if m.HandOff {
		slog.Info("Handed over to the talaria service; waiting for the service manager to stop this process", "service", setup.UnitName)
		t := time.NewTimer(handOffWait)
		defer t.Stop()
		select {
		case <-sigc:
			b.exit(0)
		case <-t.C:
			slog.Warn("The service manager did not stop this process; exiting")
			b.exit(exitRestart)
		}
		return
	}
	select {
	case sig := <-sigc:
		slog.Info("Stopping", "signal", sig.String())
		b.exit(0)
	default:
	}
}
