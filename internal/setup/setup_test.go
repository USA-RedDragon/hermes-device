package setup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

const testHub = "https://hub.example.com"

const daemonReload = "systemctl --user daemon-reload"

// workingUnit is a hand-written unit that runs the daemon as setup does.
const workingUnit = `[Unit]
Description=talaria: runs the Hermes agent's commands on this PC
After=network-online.target hermes-device.service
Conflicts=hermes-device.service

[Service]
ExecStart=%h/.talaria/bin/talaria run -c %h/.config/talaria/config.yaml
Restart=always
RestartSec=5
MemoryMax=2G
OOMPolicy=continue
TasksMax=512

[Install]
WantedBy=default.target
`

// systemd fakes a systemd user manager whose unit runs once started.
type systemd struct {
	mu     sync.Mutex
	active bool
}

func (d *systemd) respond(home string) func(string, []string) ([]byte, error) {
	return func(name string, args []string) ([]byte, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case name == systemctl && len(args) > 1 && args[1] == "is-system-running":
			return []byte("degraded\n"), errNotFound
		case name == systemctl && len(args) > 1 && args[1] == "is-active":
			if d.active {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), errNotFound
		case name == systemctl && len(args) > 1 && (args[1] == "start" || slices.Contains(args, "restart")):
			d.active = true
		case name == systemctl && len(args) > 2 && args[1] == "disable":
			d.active = false
		case name == zsh:
			path := fakeShellPath(home, ".zshrc", home+"/.local/bin:/usr/local/bin:/usr/bin:"+home+"/gone")
			return []byte("noise from .zshrc\n__TALARIA_PATH__=" + path + "\n"), nil
		case name == bash:
			return []byte("\n__TALARIA_PATH__=" + fakeShellPath(home, bashProfile, "/usr/local/bin:/usr/bin") + "\n"), nil
		}
		return nil, nil
	}
}

// fakeShellPath is base with the directories of env.sh in front, when the
// startup file sources env.sh.
func fakeShellPath(home, startup, base string) string {
	data, err := os.ReadFile(filepath.Join(home, startup))
	if err != nil || !strings.Contains(string(data), "env.sh") {
		return base
	}
	env, _ := os.ReadFile(filepath.Join(home, ".config", "shell", "env.sh"))
	for _, d := range []string{".local/bin", ".talaria/bin"} {
		if strings.Contains(string(env), `"$HOME/`+d+`"`) && !strings.Contains(":"+base+":", ":"+home+"/"+d+":") {
			base = home + "/" + d + ":" + base
		}
	}
	return base
}

type fixture struct {
	sys      *setup.System
	run      *fakeRunner
	sd       *systemd
	exe      string
	opts     *setup.Options
	paired   int
	statuses []setup.BrowserStatus
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{run: &fakeRunner{}, sd: &systemd{}}
	f.sys = newSystem(t, linux, f.run, map[string]string{systemctl: "/usr/bin/" + systemctl, "firefox": "/usr/bin/firefox"},
		map[string]string{"DISPLAY": ":0"})
	f.sys.Shell = "/usr/bin/zsh"
	f.run.respond = f.sd.respond(f.sys.Home)
	home := f.sys.Home
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".mozilla", "firefox", "abc.default", "prefs.js"), "")
	writeFile(t, filepath.Join(home, ".zshrc"), zshrcText)
	f.exe = filepath.Join(home, "Downloads", "talaria")
	writeFile(t, f.exe, "binary v0.9.0")

	xpi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/download/v0.9.0/talaria-firefox-0.9.0.xpi" {
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(xpi.Close)
	f.opts = &setup.Options{
		Version: vNew, Exe: f.exe, HubURL: testHub,
		ConfigFile: filepath.Join(home, ".config", "talaria", "config.yaml"),
		StateDir:   filepath.Join(home, ".config", "talaria"),
		Wait:       5 * time.Second, Poll: time.Millisecond,
		Releases: &setup.Releases{BaseURL: xpi.URL + "/releases", Client: xpi.Client()},
		Pair: func(context.Context) (bool, error) {
			f.paired++
			return f.paired == 1, nil
		},
		InstallHost: func(bin string) (string, error) {
			p := filepath.Join(home, ".mozilla", "native-messaging-hosts", "hermes_device.json")
			writeFile(t, p, bin)
			return p, nil
		},
		BrowserStatus: func(context.Context) (*setup.BrowserStatus, error) {
			if len(f.statuses) == 0 {
				return &setup.BrowserStatus{Enabled: true, Connected: true}, nil
			}
			st := f.statuses[0]
			f.statuses = f.statuses[1:]
			return &st, nil
		},
	}
	return f
}

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

// TestSetupUpgradesOwnUnit checks that setup replaces a unit an older
// release of setup wrote, so an existing install gets new settings such as
// OOMPolicy=continue.
func TestSetupUpgradesOwnUnit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(f.sys.Home, ".config", "systemd", "user", setup.UnitName)
	old := strings.Replace(readFile(t, unit), "OOMPolicy=continue\n", "", 1)
	writeFile(t, unit, old)
	st, err := setup.LoadState(f.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(old))
	st.ServiceSHA256 = hex.EncodeToString(sum[:])
	if err := st.Save(f.opts.StateDir); err != nil {
		t.Fatal(err)
	}
	f.sd.active = true
	f.run.Reset()
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, unit); !strings.Contains(got, "\nOOMPolicy=continue\n") {
		t.Errorf("unit not upgraded:\n%s", got)
	}
	if matches, _ := filepath.Glob(unit + ".*.bak"); len(matches) != 0 {
		t.Errorf("backed up a unit setup wrote: %q", matches)
	}
	for _, want := range []string{daemonReload, "systemctl --user --no-block restart " + setup.UnitName} {
		if !f.run.has(want) {
			t.Errorf("no %q in %q", want, f.run.Calls())
		}
	}
}

func TestSetupRefusesForeignUnitAndRecovers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	unit := filepath.Join(f.sys.Home, ".config", "systemd", "user", setup.UnitName)
	writeFile(t, unit, strings.Replace(workingUnit, "MemoryMax=2G", "MemoryMax=8G", 1))
	if _, err := f.sys.Setup(t.Context(), f.opts); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Setup with a foreign unit: %v", err)
	}
	st, err := setup.LoadState(f.opts.StateDir)
	if err != nil || st.Binary == "" || st.Config == "" || st.PairedHub != testHub || st.Service != "" {
		t.Fatalf("state after the failure = %+v, %v", st, err)
	}
	f.opts.Force = true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatalf("Setup --force: %v", err)
	}
	if !strings.Contains(readFile(t, unit), "MemoryMax=2G") || !exists(unit+".talaria-20261110-120000.bak") {
		t.Error("force did not replace the unit after a backup")
	}
	st, _ = setup.LoadState(f.opts.StateDir)
	if st.PairedHub != testHub {
		t.Errorf("the second run forgot the first run's pairing: %+v", st)
	}
}

func TestSetupWithoutServiceManager(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell = true
	f.sys.LookPath = func(string) (string, error) { return "", errNotFound }
	rep, err := f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rep.Todo, "\n")
	if !strings.Contains(joined, "no systemd user manager") || !strings.Contains(joined, "connected to talaria") {
		t.Errorf("todo %q", rep.Todo)
	}
	if f.run.has(systemctl) {
		t.Errorf("calls %q", f.run.Calls())
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

func TestShellCheckAnswerNo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoBrowser = true
	f.sys.Interactive, f.sys.In = true, strings.NewReader("\n")
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.sys.Home, ".config", "shell", "env.sh")) {
		t.Error("wrote env.sh after no")
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

func noUnpair(string) error { return nil }

func TestUninstallKeepsBinaryWhenServiceWillNotStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	f.run.respond = func(string, []string) ([]byte, error) { return nil, errNotFound }
	rep, err := f.sys.Uninstall(t.Context(), &setup.UninstallOptions{StateDir: f.opts.StateDir, Unpair: noUnpair})
	if err == nil {
		t.Fatal("no error when the service would not stop")
	}
	bin := filepath.Join(f.sys.Home, ".talaria", "bin", "talaria")
	if !exists(bin) || !exists(f.opts.ConfigFile) || len(rep.Todo) == 0 {
		t.Errorf("removed the binary or config of a running service: %+v", rep)
	}
	if st, _ := setup.LoadState(f.opts.StateDir); st.Binary == "" || st.Service == "" {
		t.Errorf("state lost what is left: %+v", st)
	}
}

func TestUninstallLeavesReplacedUnitAndRestoresBackups(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser, f.opts.Force, f.opts.OverwriteConfig = true, true, true, true
	unit := filepath.Join(f.sys.Home, ".config", "systemd", "user", setup.UnitName)
	mine := strings.Replace(workingUnit, "MemoryMax=2G", "MemoryMax=8G", 1)
	writeFile(t, unit, mine)
	writeFile(t, f.opts.ConfigFile, "log-level: debug\n")
	f.opts.ConfigExists = true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sys.Uninstall(t.Context(), &setup.UninstallOptions{StateDir: f.opts.StateDir, Unpair: noUnpair}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, unit) != mine || readFile(t, f.opts.ConfigFile) != "log-level: debug\n" {
		t.Error("uninstall did not put the user's unit and config back")
	}

	g := newFixture(t)
	g.opts.NoShell, g.opts.NoBrowser = true, true
	if _, err := g.sys.Setup(t.Context(), g.opts); err != nil {
		t.Fatal(err)
	}
	gunit := filepath.Join(g.sys.Home, ".config", "systemd", "user", setup.UnitName)
	writeFile(t, gunit, workingUnit)
	g.run.Reset()
	rep, err := g.sys.Uninstall(t.Context(), &setup.UninstallOptions{StateDir: g.opts.StateDir, Unpair: noUnpair})
	if err != nil {
		t.Fatal(err)
	}
	if !exists(gunit) || g.run.has("systemctl --user disable") || len(rep.Todo) == 0 {
		t.Errorf("uninstall touched a unit the user replaced: %q %+v", g.run.Calls(), rep)
	}
	if exists(filepath.Join(g.opts.StateDir, setup.StateFileName)) {
		t.Error("state file left behind")
	}
}

func TestSetupRestartsAfterConfigChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.opts.ConfigFile, readFile(t, f.opts.ConfigFile)+"log-level: debug\n")
	f.opts.ConfigExists = true
	f.run.Reset()
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if !f.run.has("systemctl --user --no-block restart") {
		t.Errorf("a changed config did not restart the service: %q", f.run.Calls())
	}
	f.run.Reset()
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if f.run.has("systemctl --user --no-block restart") {
		t.Error("restarted without a change")
	}
}

func TestSetupEditedOwnUnitIsBackedUp(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(f.sys.Home, ".config", "systemd", "user", setup.UnitName)
	edited := readFile(t, unit) + "Environment=FOO=bar\n"
	writeFile(t, unit, edited)
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if readFile(t, unit+".talaria-20261110-120000.bak") != edited {
		t.Error("the user's edit of setup's unit is lost")
	}
}

func TestSetupPromptStopsOnCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoBrowser = true
	r, w := io.Pipe()
	defer w.Close()
	f.sys.Interactive, f.sys.In = true, r
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := f.sys.Setup(ctx, f.opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f.run.has("systemctl --user start") {
		t.Error("went on after Ctrl-C")
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

// Uninstall leaves nothing of talaria's own behind: its cache, and on
// macOS the log that launchd wrote for the agent.
func TestUninstallRemovesCacheAndLaunchdLog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(f.sys.CacheDir, "talaria")
	writeFile(t, filepath.Join(cache, "release.json"), "{}")
	writeFile(t, filepath.Join(cache, "hermes", "builds", "x", "file"), "x")
	other := filepath.Join(f.sys.CacheDir, "other", "keep")
	writeFile(t, other, "x")
	rep, err := f.sys.Uninstall(t.Context(), &setup.UninstallOptions{StateDir: f.opts.StateDir, Unpair: noUnpair})
	if err != nil {
		t.Fatal(err)
	}
	if exists(cache) || !exists(other) || !strings.Contains(strings.Join(rep.Done, "\n"), "Removed the cache "+cache) {
		t.Errorf("cache left or other cache removed: %+v", rep)
	}

	r := &fakeRunner{respond: func(string, []string) ([]byte, error) { return nil, errNotFound }}
	sys := newSystem(t, "darwin", r, map[string]string{launchctl: binLaunchctl}, nil)
	svc, err := sys.NewService()
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(sys.Home, "Library", "Application Support", "talaria")
	content := string(svc.Content("/bin/talaria", filepath.Join(stateDir, "config.yaml")))
	writeFile(t, svc.Path(), content)
	logFile := filepath.Join(sys.Home, "Library", "Logs", "talaria.log")
	writeFile(t, logFile, "time=... msg=Running\n")
	st := &setup.State{Service: svc.Path(), ServiceSHA256: sha256Hex(content), ServiceOn: true}
	if err := st.Save(stateDir); err != nil {
		t.Fatal(err)
	}
	rep, err = sys.Uninstall(t.Context(), &setup.UninstallOptions{StateDir: stateDir, Unpair: noUnpair})
	if err != nil {
		t.Fatal(err)
	}
	if exists(svc.Path()) || exists(logFile) {
		t.Errorf("agent file or its log left: %+v", rep)
	}
}
