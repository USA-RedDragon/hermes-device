package setup_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

func TestLaunchdAgent(t *testing.T) {
	t.Parallel()
	loaded := false
	disabled := "enabled"
	r := &fakeRunner{}
	r.respond = func(_ string, args []string) ([]byte, error) {
		switch args[0] {
		case "print-disabled":
			return []byte("\tdisabled services = {\n\t\t\"com.example.other\" => disabled\n\t\t\"" + setup.LaunchdLabel + "\" => " + disabled + "\n\t}\n"), nil
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
	if !svc.Active(t.Context()) || !r.has("launchctl bootstrap gui/1000 ") || r.has("launchctl enable") {
		t.Errorf("calls %q", r.Calls())
	}
	// Only an agent that launchd has disabled gets enabled: enable leaves a
	// record in launchd's database that outlives the agent
	for _, d := range []string{"disabled", "true"} {
		disabled = d
		r.Reset()
		if err := svc.Load(t.Context()); err != nil || !r.has("launchctl enable gui/1000/"+setup.LaunchdLabel) {
			t.Errorf("disabled %s: %v %q", d, err, r.Calls())
		}
	}
	disabled = "enabled"
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
