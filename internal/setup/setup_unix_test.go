//go:build unix

package setup_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

func TestSetupInstallsEverythingAndUninstallUndoesIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	home := f.sys.Home
	f.sys.Interactive, f.sys.In = true, strings.NewReader("y\n"+"y\n")
	f.statuses = []setup.BrowserStatus{
		{Enabled: true}, {Enabled: true},
	}
	rep, err := f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatalf("Setup: %v\n%+v", err, rep)
	}

	checkInstalled(t, f, rep)
	checkSecondRun(t, f)

	bin := filepath.Join(home, ".talaria", "bin", "talaria")
	f.run.Reset()
	var removed []string
	_, err = f.sys.Uninstall(t.Context(), &setup.UninstallOptions{
		StateDir: f.opts.StateDir,
		RemoveHost: func() error {
			return os.Remove(filepath.Join(home, ".mozilla", "native-messaging-hosts", "hermes_device.json"))
		},
		Unpair: func(hub string) error {
			removed = append(removed, hub)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !f.run.has("systemctl --user disable --now " + setup.UnitName) {
		t.Errorf("calls %q", f.run.Calls())
	}
	for _, p := range []string{
		bin, filepath.Join(home, ".talaria"), f.opts.ConfigFile, filepath.Join(home, ".config", "systemd", "user", setup.UnitName),
		filepath.Join(home, ".config", "shell", "env.sh"), filepath.Join(home, bashProfile),
		filepath.Join(home, ".mozilla", "native-messaging-hosts", "hermes_device.json"), filepath.Join(f.opts.StateDir, setup.StateFileName),
	} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if got := readFile(t, filepath.Join(home, ".zshrc")); got != zshrcText {
		t.Errorf(".zshrc after uninstall = %q", got)
	}
	if !exists(filepath.Join(home, ".zshrc.talaria-20261110-120000.bak")) {
		t.Error("uninstall removed the backup")
	}
	if len(removed) != 1 || removed[0] != testHub {
		t.Errorf("unpaired %q", removed)
	}
}

func TestSetupKeepsUserFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	home := f.sys.Home
	f.opts.NoShell = true
	userCfg := "hub:\n  url: " + testHub + "\ndevice:\n  roots: [/srv]\n"
	writeFile(t, f.opts.ConfigFile, userCfg)
	f.opts.ConfigExists, f.opts.BrowserEnabled = true, false
	unit := filepath.Join(home, ".config", "systemd", "user", setup.UnitName)
	writeFile(t, unit, workingUnit)
	f.sd.active = true

	rep, err := f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if readFile(t, f.opts.ConfigFile) != userCfg {
		t.Error("setup changed the user's config")
	}
	if readFile(t, unit) != workingUnit {
		t.Error("setup rewrote an equivalent unit")
	}
	if !f.run.has("systemctl --user --no-block restart") {
		t.Error("a new binary did not restart the service")
	}
	if len(f.run.started) != 0 {
		t.Errorf("opened Firefox with browser.enabled off: %q", f.run.started)
	}
	if len(rep.Todo) != 1 || !strings.Contains(rep.Todo[0], "browser.enabled is off") {
		t.Errorf("todo %q", rep.Todo)
	}

	rep, err = f.sys.Uninstall(t.Context(), &setup.UninstallOptions{StateDir: f.opts.StateDir, Unpair: func(string) error { return nil }})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !exists(unit) || !exists(f.opts.ConfigFile) {
		t.Errorf("uninstall removed files setup did not write: %+v", rep)
	}
}

// TestSetupReportsFailure is a foreign unit in the way: the summary says
// setup failed, not that nothing is left to do.
func TestSetupReportsFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	writeFile(t, filepath.Join(f.sys.Home, ".config", "systemd", "user", setup.UnitName), "[Service]\nExecStart=/opt/x\n")
	rep, err := f.sys.Setup(t.Context(), f.opts)
	if !errors.Is(err, setup.ErrForeignService) {
		t.Fatalf("err = %v", err)
	}
	out := &bytes.Buffer{}
	f.sys.Out = out
	rep.Print(f.sys, err)
	if strings.Contains(out.String(), "Nothing is left to do") || !strings.Contains(out.String(), "Failed:\n  - "+err.Error()+"\n") {
		t.Errorf("summary:\n%s", out)
	}
	out.Reset()
	rep.Print(f.sys, nil)
	if !strings.Contains(out.String(), "Nothing is left to do") || strings.Contains(out.String(), "Failed") {
		t.Errorf("summary without an error:\n%s", out)
	}
}

func TestShellCheckNonInteractiveChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoBrowser = true
	rep, err := f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(f.sys.Home, ".zshrc")) != zshrcText || exists(filepath.Join(f.sys.Home, bashProfile)) {
		t.Error("changed dotfiles without a yes")
	}
	if len(rep.Todo) != 2 || !strings.Contains(rep.Todo[0], filepath.Join(f.sys.Home, ".talaria", "bin")+" to PATH (run setup in a terminal") ||
		!strings.Contains(rep.Todo[1], filepath.Join(f.sys.Home, ".local", "bin")) {
		t.Errorf("todo %q", rep.Todo)
	}
}

// checkSecondRun runs setup again: it changes nothing and asks nothing.
func checkSecondRun(t *testing.T, f *fixture) {
	t.Helper()
	home := f.sys.Home
	f.run.Reset()
	f.sys.In = strings.NewReader("")
	rep, err := f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	for _, c := range f.run.Calls() {
		if strings.Contains(c, "restart") || strings.Contains(c, "start ") || strings.Contains(c, "enable") {
			t.Errorf("second run called %q", c)
		}
	}
	if strings.Count(readFile(t, filepath.Join(home, ".zshrc")), "talaria setup >>>") != 1 ||
		strings.Count(readFile(t, filepath.Join(home, ".config", "shell", "env.sh")), "talaria setup >>>") != 2 {
		t.Error("second run added another block")
	}
	if len(rep.Todo) != 0 {
		t.Errorf("second report %+v", rep)
	}
}

func checkInstalled(t *testing.T, f *fixture, rep *setup.Report) {
	t.Helper()
	home := f.sys.Home
	bin := filepath.Join(home, ".talaria", "bin", "talaria")
	if got := readFile(t, bin); got != "binary v0.9.0" {
		t.Errorf("binary = %q", got)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", fi.Mode())
	}
	cfg := readFile(t, f.opts.ConfigFile)
	for _, want := range []string{"url: " + testHub, "- " + home, "enabled: true"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	unit := readFile(t, filepath.Join(home, ".config", "systemd", "user", setup.UnitName))
	if !strings.HasPrefix(unit, "# Written by talaria setup") || !strings.HasSuffix(unit, workingUnit) {
		t.Errorf("unit:\n%s", unit)
	}
	for _, want := range []string{daemonReload, "systemctl --user enable " + setup.UnitName, "systemctl --user start " + setup.UnitName} {
		if !f.run.has(want) {
			t.Errorf("no %q in %q", want, f.run.Calls())
		}
	}
	if f.run.has("systemctl --user --no-block restart") {
		t.Error("restarted a service it had just started")
	}
	if len(f.run.started) != 1 || !strings.HasPrefix(f.run.started[0], "/usr/bin/firefox http") || !strings.HasSuffix(f.run.started[0], "/releases/download/v0.9.0/talaria-firefox-0.9.0.xpi") {
		t.Errorf("started %q", f.run.started)
	}
	env := readFile(t, filepath.Join(home, ".config", "shell", "env.sh"))
	if !strings.Contains(env, `for talaria_dir in "$HOME/.local/bin"; do`) || strings.Contains(env, "gone") ||
		!strings.Contains(env, `for talaria_dir in "$HOME/.talaria/bin"; do`) || strings.Count(env, "# >>> talaria setup >>>") != 2 {
		t.Errorf("env.sh:\n%s", env)
	}
	zshrc := readFile(t, filepath.Join(home, ".zshrc"))
	if !strings.HasPrefix(zshrc, "export EDITOR=vi\n\n# >>> talaria setup >>>\n") || !strings.Contains(zshrc, `. "$HOME/.config/shell/env.sh"`) {
		t.Errorf(".zshrc:\n%s", zshrc)
	}
	if got := readFile(t, filepath.Join(home, ".zshrc.talaria-20261110-120000.bak")); got != zshrcText {
		t.Errorf("backup = %q", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, bashProfile)), "env.sh") {
		t.Error(".bash_profile does not source env.sh")
	}
	out, _ := f.sys.Out.(*bytes.Buffer)
	if !strings.Contains(out.String(), "Make this change? [y/N]") {
		t.Errorf("no prompt in %q", out)
	}
	var done strings.Builder
	for _, d := range rep.Done {
		done.WriteString(d + "\n")
	}
	if !strings.Contains(done.String(), "extension is connected to talaria") || len(rep.Todo) != 0 {
		t.Errorf("report %+v", rep)
	}
}
