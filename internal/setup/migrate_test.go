//go:build unix

package setup_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

const legacyUnit = `[Unit]
Description=hermes-device: runs the Hermes agent's commands on this PC
After=network-online.target

[Service]
ExecStart=%h/.hermes-device/bin/hermes-device run -c %h/.config/hermes-device/config.yaml
Restart=always
MemoryMax=8G

[Install]
WantedBy=default.target
`

type legacyLayout struct {
	sys                       *setup.System
	run                       *fakeRunner
	legacyBin, bin            string
	legacyCfg, cfg            string
	legacyUnitPath, unitPath  string
	legacyManifest, manifest  string
	legacySocket, socket      string
	legacyCache, cache, alias string
}

// newLegacyLayout writes what a release before the rename installed, with a
// systemd user manager whose legacy unit is enabled and active.
func newLegacyLayout(t *testing.T) *legacyLayout {
	t.Helper()
	r := &fakeRunner{respond: func(name string, args []string) ([]byte, error) {
		if name != systemctl {
			return nil, nil
		}
		switch strings.Join(args, " ") {
		case "--user is-system-running":
			return []byte("running\n"), nil
		case "--user is-enabled hermes-device.service":
			return []byte("enabled\n"), nil
		case "--user is-active hermes-device.service":
			return []byte("active\n"), nil
		}
		return nil, nil
	}}
	sys := newSystem(t, linux, r, map[string]string{systemctl: "/usr/bin/systemctl"}, nil)
	h := sys.Home
	l := &legacyLayout{
		sys: sys, run: r,
		legacyBin: filepath.Join(h, ".hermes-device", "bin", "hermes-device"), bin: filepath.Join(h, ".talaria", "bin", "talaria"),
		alias:     filepath.Join(h, ".talaria", "bin", "tal"),
		legacyCfg: filepath.Join(h, ".config", "hermes-device"), cfg: filepath.Join(h, ".config", "talaria"),
		legacyCache: filepath.Join(h, ".cache", "hermes-device"), cache: filepath.Join(h, ".cache", "talaria"),
		legacyUnitPath: filepath.Join(h, ".config", "systemd", "user", "hermes-device.service"),
		unitPath:       filepath.Join(h, ".config", "systemd", "user", "talaria.service"),
		legacyManifest: filepath.Join(h, ".mozilla", "native-messaging-hosts", "hermes_device.json"),
		manifest:       filepath.Join(h, ".mozilla", "native-messaging-hosts", "talaria.json"),
		legacySocket:   filepath.Join(h, ".mozilla", "native-messaging-hosts", "hermes_device.socket"),
		socket:         filepath.Join(h, ".mozilla", "native-messaging-hosts", "talaria.socket"),
	}
	writeFile(t, l.legacyBin, "binary")
	if err := os.Chmod(l.legacyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(l.legacyCfg, "config.yaml"), "hub:\n  url: https://hub.example.com\nstore:\n  token-file: "+l.legacyCfg+"/token\n  state-dir: "+l.legacyCfg+"\n")
	writeFile(t, filepath.Join(l.legacyCfg, "daemon.json"), "{}")
	writeFile(t, filepath.Join(l.legacyCache, "release.json"), "{}")
	writeFile(t, l.legacyUnitPath, legacyUnit)
	writeFile(t, l.legacyUnitPath+".d/override.conf", "[Service]\nNice=5\n")
	writeFile(t, l.legacyUnitPath+".d/exec.conf", "[Service]\nWorkingDirectory=%h/.config/hermes-device\n")
	writeFile(t, l.legacyManifest, `{"name":"hermes_device","description":"hermes-device browser control","path":"`+l.legacyBin+
		`","type":"stdio","allowed_extensions":["browser@hermes-device"]}`)
	writeFile(t, l.legacySocket, filepath.Join(l.legacyCfg, "browser.sock")+"\n")
	return l
}

func TestMigrateMovesEverything(t *testing.T) {
	t.Parallel()
	l := newLegacyLayout(t)
	if !l.sys.NeedsMigration() {
		t.Fatal("NeedsMigration = false for a legacy layout")
	}
	m, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{UnderLegacyService: true})
	if err != nil {
		t.Fatalf("Migrate: %v\n%+v", err, m)
	}
	if !m.HandOff {
		t.Error("no hand-over to the talaria service")
	}
	if readFile(t, l.bin) != "binary" {
		t.Error("binary not moved")
	}
	if !slices.Contains(m.Notes, `~/.talaria/bin is not on PATH, so the talaria and tal commands are "not found" in a terminal. Run ~/.talaria/bin/talaria setup to add it.`) {
		t.Errorf("no PATH note in %q", m.Notes)
	}
	for link, want := range map[string]string{l.legacyBin: l.bin, l.alias: "talaria"} {
		if got, err := os.Readlink(link); err != nil || got != want {
			t.Errorf("%s links to %q (%v), want %q", link, got, err, want)
		}
	}
	if exists(l.legacyCfg) || exists(l.legacyCache) || !exists(filepath.Join(l.cache, "release.json")) {
		t.Error("config or cache directory not moved")
	}
	if got := readFile(t, filepath.Join(l.cfg, "config.yaml")); !strings.Contains(got, l.cfg+"/token") || !strings.Contains(got, "state-dir: "+l.cfg+"\n") {
		t.Errorf("config paths not moved:\n%s", got)
	}
	unit := readFile(t, l.unitPath)
	for _, want := range []string{
		"ExecStart=%h/.talaria/bin/talaria run -c %h/.config/talaria/config.yaml", "MemoryMax=8G",
		"[Service]\nOOMPolicy=continue\n", "Description=talaria:", "Conflicts=hermes-device.service", "After=hermes-device.service",
		"# Moved from hermes-device by talaria.",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if got := readFile(t, l.unitPath+".d/exec.conf"); got != "[Service]\nWorkingDirectory=%h/.config/talaria\n" {
		t.Errorf("drop-in not rewritten: %q", got)
	}
	if exists(l.legacyUnitPath) || readFile(t, l.unitPath+".d/override.conf") != "[Service]\nNice=5\n" {
		t.Error("legacy unit or its drop-in not moved")
	}
	calls := l.run.Calls()
	last := -1
	for _, want := range []string{
		daemonReload, "systemctl --user enable talaria.service",
		"systemctl --user --no-block start talaria.service", "systemctl --user disable hermes-device.service",
	} {
		i := slices.Index(calls, want)
		if i <= last {
			t.Errorf("%q missing or out of order in %q", want, calls)
		}
		last = i
	}
	sock := filepath.Join(l.cfg, "browser.sock") + "\n"
	for _, mf := range []string{l.manifest, l.legacyManifest} {
		var hm struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(readFile(t, mf)), &hm); err != nil || hm.Path != l.bin {
			t.Errorf("%s: %+v, %v", mf, hm, err)
		}
	}
	if readFile(t, l.socket) != sock || readFile(t, l.legacySocket) != sock {
		t.Error("socket files do not name the moved socket")
	}
	if l.sys.NeedsMigration() {
		t.Error("NeedsMigration after Migrate")
	}
	again, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{})
	if err != nil || !again.Empty() {
		t.Errorf("second Migrate = %+v, %v", again, err)
	}
}

func TestMigrateKeepsWhatIsThere(t *testing.T) {
	t.Parallel()
	l := newLegacyLayout(t)
	writeFile(t, l.bin, "new")
	writeFile(t, filepath.Join(l.cfg, "config.yaml"), "hub:\n  url: https://hub.example.com\n")
	writeFile(t, l.unitPath, "[Service]\nExecStart=/x\n")
	m, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if readFile(t, l.bin) != "new" || readFile(t, l.unitPath) != "[Service]\nExecStart=/x\n" {
		t.Error("replaced a talaria file that was already there")
	}
	if !exists(filepath.Join(l.legacyCfg, "config.yaml")) || !exists(filepath.Join(l.cfg, "daemon.json")) {
		t.Error("config directory merge wrong")
	}
	if m.HandOff {
		t.Error("hand-over without UnderLegacyService")
	}
	if !slices.Contains(l.run.Calls(), "systemctl --user --no-block start talaria.service") {
		t.Errorf("did not start the talaria unit in place of the active legacy one: %q", l.run.Calls())
	}
}

func TestMigrateRecordsState(t *testing.T) {
	t.Parallel()
	l := newLegacyLayout(t)
	st := &setup.State{Binary: l.legacyBin, Config: filepath.Join(l.legacyCfg, "config.yaml"), Service: l.legacyUnitPath,
		ServiceSHA256: sha256Hex(legacyUnit), NativeHost: l.legacyManifest}
	if err := st.Save(l.legacyCfg); err != nil {
		t.Fatal(err)
	}
	if _, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := setup.LoadState(l.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Binary != l.bin || got.Config != filepath.Join(l.cfg, "config.yaml") || got.Service != l.unitPath ||
		got.NativeHost != l.manifest || got.ServiceSHA256 != sha256Hex(readFile(t, l.unitPath)) {
		t.Errorf("state = %+v", got)
	}
}

func TestMigratedConfigPath(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, nil)
	old := filepath.Join(sys.ConfigDir, "hermes-device", "config.yaml")
	if got := sys.MigratedConfigPath(old); got != filepath.Join(sys.ConfigDir, "talaria", "config.yaml") {
		t.Errorf("MigratedConfigPath = %s", got)
	}
	if got := sys.MigratedConfigPath("/etc/x.yaml"); got != "/etc/x.yaml" {
		t.Errorf("MigratedConfigPath = %s", got)
	}
}

func TestMigrateLaunchdUnderLegacyAgent(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{respond: func(name string, args []string) ([]byte, error) {
		if name == launchctl && args[0] == lcPrint && strings.HasSuffix(args[1], "talaria") {
			return nil, errNotFound
		}
		return nil, nil
	}}
	sys := newSystem(t, "darwin", r, map[string]string{launchctl: binLaunchctl}, nil)
	legacy := filepath.Join(sys.Home, "Library", "LaunchAgents", "io.github.usa-reddragon.hermes-device.plist")
	writeFile(t, legacy, `<plist><dict><key>Label</key><string>io.github.usa-reddragon.hermes-device</string>`+
		`<string>`+filepath.Join(sys.Home, ".hermes-device", "bin", "hermes-device")+`</string>`+
		`<string>`+filepath.Join(sys.Home, "Library", "Logs", "hermes-device.log")+`</string></dict></plist>`)
	m, err := sys.Migrate(t.Context(), setup.MigrateOptions{UnderLegacyService: true})
	if err != nil || !m.HandOff {
		t.Fatalf("Migrate = %+v, %v", m, err)
	}
	plist := readFile(t, filepath.Join(sys.Home, "Library", "LaunchAgents", "io.github.usa-reddragon.talaria.plist"))
	for _, want := range []string{"<string>io.github.usa-reddragon.talaria</string>", "/.talaria/bin/talaria<", "talaria.log"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q: %s", want, plist)
		}
	}
	calls := r.Calls()
	bi := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "launchctl bootstrap gui/1000 ") })
	bo := slices.Index(calls, "launchctl bootout gui/1000/io.github.usa-reddragon.hermes-device")
	if bi < 0 || bo < bi {
		t.Errorf("want bootstrap of the talaria agent before the bootout of the legacy one: %q", calls)
	}
}

func TestMigrateKeepsRunningLegacyService(t *testing.T) {
	t.Parallel()
	l := newLegacyLayout(t)
	m, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{KeepService: true})
	if err != nil || m.HandOff {
		t.Fatalf("Migrate = %+v, %v", m, err)
	}
	if !exists(l.legacyUnitPath) || exists(l.unitPath) || !exists(l.bin) || !exists(l.cfg) {
		t.Error("want files moved and the service left alone")
	}
	for _, c := range l.run.Calls() {
		if strings.Contains(c, "start") || strings.Contains(c, "disable") {
			t.Errorf("touched the service: %q", c)
		}
	}
}

func TestMigrateHostNeedsTheBinary(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, nil)
	mf := filepath.Join(sys.Home, ".mozilla", "native-messaging-hosts", "hermes_device.json")
	body := `{"name":"hermes_device","path":"/opt/bin/hermes-device","type":"stdio","allowed_extensions":["browser@hermes-device"]}`
	writeFile(t, mf, body)
	if sys.NeedsMigration() {
		t.Error("NeedsMigration for a host manifest of a binary that setup did not install")
	}
	if _, err := sys.Migrate(t.Context(), setup.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, mf) != body || exists(filepath.Join(filepath.Dir(mf), "talaria.json")) {
		t.Error("rewrote the host manifest without a talaria binary")
	}
}

func TestMigrateNoPathNoteWhenOnPath(t *testing.T) {
	t.Parallel()
	l := newLegacyLayout(t)
	l.sys.Getenv = func(k string) string {
		if k == "PATH" {
			return "/usr/bin:" + filepath.Dir(l.bin)
		}
		return ""
	}
	m, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{KeepService: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range m.Notes {
		if strings.Contains(n, "PATH") {
			t.Errorf("note %q", n)
		}
	}
}

// migratedUnit is a unit that Migrate wrote, before it marked them, from a
// hand-written legacy unit.
const migratedUnit = `[Unit]
After=hermes-device.service
Conflicts=hermes-device.service
Description=talaria: runs the Hermes agent's commands on this PC
After=network-online.target

[Service]
ExecStart=%h/.talaria/bin/talaria run -c %h/.config/talaria/config.yaml
Restart=always

[Install]
WantedBy=default.target
`

// TestSetupReplacesMigratedUnits runs setup over a unit Migrate wrote now
// and one it wrote before it marked them: setup treats both as its own,
// backs them up and leaves their drop-ins alone.
func TestSetupReplacesMigratedUnits(t *testing.T) {
	t.Parallel()
	l := newLegacyLayout(t)
	if _, err := l.sys.Migrate(t.Context(), setup.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, unit := range map[string]string{"marked": readFile(t, l.unitPath), "unmarked": migratedUnit} {
		f := newFixture(t)
		f.opts.NoShell, f.opts.NoBrowser = true, true
		path := filepath.Join(f.sys.Home, ".config", "systemd", "user", setup.UnitName)
		dropIn := filepath.Join(path+".d", "oom.conf")
		writeFile(t, path, unit)
		writeFile(t, dropIn, "[Service]\nOOMPolicy=continue\n")
		rep, err := f.sys.Setup(t.Context(), f.opts)
		if err != nil {
			t.Fatalf("%s: Setup: %v", name, err)
		}
		if got := readFile(t, path); !strings.HasPrefix(got, "# Written by talaria setup") {
			t.Errorf("%s: unit not replaced:\n%s", name, got)
		}
		if readFile(t, path+".talaria-20261110-120000.bak") != unit || readFile(t, dropIn) != "[Service]\nOOMPolicy=continue\n" {
			t.Errorf("%s: backup or drop-in wrong", name)
		}
		if !strings.Contains(strings.Join(rep.Done, "\n"), "Replaced "+path) {
			t.Errorf("%s: report %+v", name, rep)
		}
	}
}

func TestMigrateMarksLaunchdAgent(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, "darwin", &fakeRunner{}, map[string]string{launchctl: binLaunchctl}, nil)
	legacy := filepath.Join(sys.Home, "Library", "LaunchAgents", "io.github.usa-reddragon.hermes-device.plist")
	writeFile(t, legacy, "<?xml version=\"1.0\"?>\n<plist><dict><key>Label</key><string>io.github.usa-reddragon.hermes-device</string></dict></plist>\n")
	if _, err := sys.Migrate(t.Context(), setup.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(sys.Home, "Library", "LaunchAgents", "io.github.usa-reddragon.talaria.plist"))
	if !strings.HasPrefix(got, "<?xml version=\"1.0\"?>\n<!-- Moved from hermes-device by talaria.") {
		t.Errorf("plist:\n%s", got)
	}
}
