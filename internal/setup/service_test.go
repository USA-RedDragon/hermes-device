package setup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/setup"
)

func TestSystemdUnitPaths(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, map[string]string{"XDG_CONFIG_HOME": "/xdg"})
	svc, err := sys.NewService()
	if err != nil {
		t.Fatal(err)
	}
	if svc.Path() != "/xdg/systemd/user/"+setup.UnitName {
		t.Errorf("path %s", svc.Path())
	}
	unit := string(svc.Content("/opt/my apps/talaria", sys.Home+"/50%/config.yaml"))
	if !strings.Contains(unit, `ExecStart="/opt/my apps/talaria" run -c %h/50%%/config.yaml`+"\n") {
		t.Errorf("unit:\n%s", unit)
	}
}

func TestLaunchdAgent(t *testing.T) {
	t.Parallel()
	loaded := false
	r := &fakeRunner{}
	r.respond = func(_ string, args []string) ([]byte, error) {
		switch args[0] {
		case lcPrint:
			if loaded {
				return []byte("state = running\n"), nil
			}
			return nil, errNotFound
		case "bootstrap":
			loaded = true
		case "bootout":
			loaded = false
		}
		return nil, nil
	}
	sys := newSystem(t, "darwin", r, map[string]string{launchctl: binLaunchctl}, nil)
	svc, err := sys.NewService()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(sys.Home, ".talaria", "bin", "talaria")
	cfg := filepath.Join(sys.Home, "Library", "Application Support", "talaria", "config.yaml")
	res, err := sys.WriteServiceFile(svc, bin, cfg, "", false)
	if err != nil || !res.Wrote {
		t.Fatalf("WriteServiceFile = %+v, %v", res, err)
	}
	plist := readFile(t, filepath.Join(sys.Home, "Library", "LaunchAgents", setup.LaunchdLabel+".plist"))
	for _, want := range []string{"<string>" + bin + "</string>", "<string>" + cfg + "</string>", "<key>KeepAlive</key>\n\t<true/>", "talaria.log"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !svc.Active(t.Context()) || !r.has("launchctl bootstrap gui/1000 ") || !r.has("launchctl enable gui/1000/"+setup.LaunchdLabel) {
		t.Errorf("calls %q", r.Calls())
	}
	if err := svc.Load(t.Context()); err != nil || !r.has("launchctl bootout gui/1000/"+setup.LaunchdLabel) {
		t.Errorf("reload: %v %q", err, r.Calls())
	}
	if err := svc.Restart(t.Context()); err != nil || !r.has("launchctl kickstart -k gui/1000/"+setup.LaunchdLabel) {
		t.Errorf("restart: %v", err)
	}
	if err := svc.Remove(t.Context()); err != nil || svc.Active(t.Context()) {
		t.Errorf("remove: %v", err)
	}
}

func TestRemoveBlocks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "rc")
	writeFile(t, p, "a\n\n# >>> hermes-device setup >>>\nx\n# <<< hermes-device setup <<<\nb\n")
	if changed, err := setup.RemoveBlocks(p, false); err != nil || !changed || readFile(t, p) != "a\nb\n" {
		t.Errorf("RemoveBlocks = %t, %v, %q", changed, err, readFile(t, p))
	}
	writeFile(t, p, "a\n# >>> hermes-device setup >>>\nx\n")
	if _, err := setup.RemoveBlocks(p, false); err == nil || readFile(t, p) != "a\n# >>> hermes-device setup >>>\nx\n" {
		t.Errorf("unterminated block: %v", err)
	}
	if changed, err := setup.RemoveBlocks(filepath.Join(dir, "missing"), true); err != nil || changed {
		t.Errorf("missing file: %t, %v", changed, err)
	}
}

func TestShellFixKeepsSymlinkedDotfiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	home := f.sys.Home
	target := filepath.Join(home, "dotfiles", "zshrc")
	writeFile(t, target, zshrcText)
	if err := os.Remove(filepath.Join(home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".profile"), "umask 022\n")
	rep := &setup.ShellReport{Shell: "zsh", Missing: []string{filepath.Join(home, ".local", "bin"), "/opt/a b/bin"}}
	plan := f.sys.PlanShell(rep)
	if len(plan.Sources) != 2 || plan.Sources[0] != filepath.Join(home, ".profile") {
		t.Fatalf("plan %+v", plan)
	}
	st := &setup.State{}
	if err := f.sys.FixShell(rep, plan, st); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(home, ".zshrc")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error(".zshrc is no longer a symbolic link")
	}
	if !strings.Contains(readFile(t, target), "env.sh") {
		t.Error("the link's target lacks the source line")
	}
	env := readFile(t, filepath.Join(home, ".config", "shell", "env.sh"))
	if !strings.Contains(env, `for hermes_device_dir in "/opt/a b/bin" "$HOME/.local/bin"; do`) {
		t.Errorf("env.sh:\n%s", env)
	}
	if len(st.Edits) != 3 || !st.Edits[0].Created || st.Edits[1].Backup == "" {
		t.Errorf("edits %+v", st.Edits)
	}
	if again := f.sys.PlanShell(rep); len(again.Sources) != 0 {
		t.Errorf("second plan %+v", again)
	}
}

func TestCheckShellFish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &fakeRunner{respond: func(name string, _ []string) ([]byte, error) {
		if name == "fish" {
			return []byte("__HERMES_DEVICE_PATH__=" + dir + ":/usr/bin\n"), nil
		}
		return []byte("__HERMES_DEVICE_PATH__=/usr/bin\n"), nil
	}}
	sys := newSystem(t, linux, r, nil, nil)
	sys.Shell = "/usr/bin/fish"
	rep, err := sys.CheckShell(t.Context())
	if err != nil || rep.Shell != "fish" || len(rep.Missing) != 1 || rep.Missing[0] != dir {
		t.Errorf("CheckShell = %+v, %v", rep, err)
	}
	sys.Shell = "/bin/bash"
	if rep, err := sys.CheckShell(t.Context()); err != nil || rep.Shell != "" {
		t.Errorf("bash login shell: %+v, %v", rep, err)
	}
	r.respond = func(string, []string) ([]byte, error) { return []byte("no marker"), errNotFound }
	sys.Shell = "/usr/bin/zsh"
	if _, err := sys.CheckShell(t.Context()); err == nil {
		t.Error("no error without a PATH line")
	}
}

func TestFirefoxExtensionInstalled(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, nil)
	prof := filepath.Join(sys.Home, ".mozilla", "firefox", "x.default")
	writeFile(t, filepath.Join(prof, "prefs.js"), "")
	ff := sys.FindFirefox()
	if !ff.Found() || ff.ExtensionInstalled() || len(ff.Command) != 0 {
		t.Errorf("before: %+v", ff)
	}
	writeFile(t, filepath.Join(prof, "extensions.json"), `{"addons":[{"id":"`+setup.ExtensionID+`","userDisabled":false}]}`)
	if !sys.FindFirefox().ExtensionInstalled() {
		t.Error("installed extension not found")
	}
}

func TestSetupOnMacStartsAgentOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoShell, f.opts.NoBrowser = true, true
	loaded := false
	f.run.respond = func(name string, args []string) ([]byte, error) {
		if name != "launchctl" {
			return nil, nil
		}
		switch args[0] {
		case lcPrint:
			if loaded {
				return []byte("state = running\n"), nil
			}
			return nil, errNotFound
		case "bootstrap":
			loaded = true
		case "bootout":
			loaded = false
		}
		return nil, nil
	}
	f.sys.GOOS = "darwin"
	f.sys.LookPath = func(string) (string, error) { return "/bin/launchctl", nil }
	if _, err := f.sys.Setup(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	if f.run.has("launchctl kickstart") {
		t.Errorf("kickstarted an agent that bootstrap had just started: %q", f.run.Calls())
	}
	if st, _ := setup.LoadState(f.opts.StateDir); !st.ServiceOn {
		t.Error("ServiceOn not recorded")
	}
}
