package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stateActive is systemctl is-active's answer for a running unit.
const stateActive = "active"

// Service names.
const (
	UnitName     = "talaria.service"
	LaunchdLabel = "io.github.usa-reddragon.talaria"
)

// Service names before the rename to talaria. Migrate replaces them.
const (
	LegacyUnitName     = "hermes-device.service"
	LegacyLaunchdLabel = "io.github.usa-reddragon.hermes-device"
)

// serviceMarker starts every service file setup writes,
// legacyServiceMarker every one that setup wrote before the rename, and
// migratedServiceMarker every one that Migrate wrote from a legacy file
// that setup did not write.
const (
	serviceMarker         = "Written by talaria setup"
	legacyServiceMarker   = "Written by hermes-device setup"
	migratedServiceMarker = "Moved from hermes-device by talaria"
)

// migratedUnitLines follow [Unit] in every unit Migrate wrote, also in
// those it wrote before it added migratedServiceMarker.
const migratedUnitLines = "[Unit]\nAfter=" + LegacyUnitName + "\nConflicts=" + LegacyUnitName + "\n"

// ownService reports whether setup or Migrate wrote the service file data.
func ownService(data []byte) bool {
	for _, m := range []string{serviceMarker, legacyServiceMarker, migratedServiceMarker, migratedUnitLines} {
		if bytes.Contains(data, []byte(m)) {
			return true
		}
	}
	return false
}

// ErrNoServiceManager is returned when the computer has no systemd user
// manager or launchd that setup can use.
var ErrNoServiceManager = errors.New("no systemd user manager, launchd or Task Scheduler to run talaria run")

// ErrForeignService is returned when a service file that setup did not
// write is in the way.
var ErrForeignService = errors.New("a service file that setup did not write is in the way")

// Service is the background service that runs talaria run.
type Service interface {
	// Path is the unit or agent file.
	Path() string
	// Name is how the summary names the service.
	Name() string
	// Available checks that the service manager runs.
	Available(ctx context.Context) error
	// Content is the file for the binary bin and the config file cfg.
	Content(bin, cfg string) []byte
	// Load makes the service manager read the file and start the service
	// at login. LoadStarts reports whether Load also starts it.
	Load(ctx context.Context) error
	LoadStarts() bool
	// Active reports whether the service runs.
	Active(ctx context.Context) bool
	// Start starts the service; Restart restarts it.
	Start(ctx context.Context) error
	Restart(ctx context.Context) error
	// Remove stops the service and disables it at login.
	Remove(ctx context.Context) error
	// Logs is a command that shows the service's logs.
	Logs() string
}

// NewService returns the service for s.GOOS.
func (s *System) NewService() (Service, error) {
	switch s.GOOS {
	case goosLinux:
		return &systemdService{s: s}, nil
	case darwin:
		return &launchdService{s: s}, nil
	case goosWindows:
		return &taskService{s: s}, nil
	default:
		return nil, ErrNoServiceManager
	}
}

// ServiceFile is the result of WriteServiceFile.
type ServiceFile struct {
	// Wrote is true when setup wrote the file; Adopted is true when a
	// file setup did not write already does the same.
	Wrote, Adopted bool
	Backup         string
}

// WriteServiceFile writes the service file for bin and cfg. A file that
// setup wrote (its hash is ours, the hash setup recorded) is replaced; one
// the user edited since, or one Migrate wrote, is backed up first. A file setup did not write is
// kept when it has the same settings, and is replaced after a backup only
// with force.
func (s *System) WriteServiceFile(svc Service, bin, cfg, ours string, force bool) (ServiceFile, error) {
	want := svc.Content(bin, cfg)
	old, err := os.ReadFile(svc.Path())
	var res ServiceFile
	foreign := err == nil && !ownService(old)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return res, err
	case bytes.Equal(old, want):
		return res, nil
	case foreign && sameSettings(old, want):
		res.Adopted = true
		return res, nil
	case foreign && !force:
		return res, fmt.Errorf("%w: %s; move it away or pass --force (setup backs it up first)", ErrForeignService, svc.Path())
	case foreign || sha256Hex(old) != ours:
		if res.Backup, err = s.backup(svc.Path()); err != nil {
			return res, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(svc.Path()), 0o755); err != nil {
		return res, err
	}
	res.Wrote = true
	return res, writeFileAtomic(svc.Path(), want, 0o644)
}

// sameSettings compares two unit or plist files without comments, blank
// lines and indentation.
func sameSettings(a, b []byte) bool {
	norm := func(d []byte) string {
		var out []string
		for line := range strings.SplitSeq(string(d), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "<!--") {
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	return norm(a) == norm(b)
}

type systemdService struct{ s *System }

func (d *systemdService) Path() string { return filepath.Join(d.dir(), UnitName) }

func (d *systemdService) dir() string {
	base := d.s.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(d.s.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

func (d *systemdService) Name() string { return "systemd user service " + UnitName }

func (d *systemdService) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	return d.s.Run.Output(ctx, nil, "systemctl", append([]string{"--user"}, args...)...)
}

func (d *systemdService) Available(ctx context.Context) error {
	if _, err := d.s.LookPath("systemctl"); err != nil {
		return ErrNoServiceManager
	}
	// is-system-running exits non-zero for "degraded", which still runs
	// units; only an empty answer means there is no user manager.
	out, _ := d.systemctl(ctx, "is-system-running")
	switch strings.TrimSpace(string(out)) {
	case "", "offline", "unknown":
		return ErrNoServiceManager
	}
	return nil
}

// unitPath writes p for a unit file: %h for the home directory, %% for %,
// and quotes when p has a space.
func (d *systemdService) unitPath(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	home := strings.ReplaceAll(d.s.Home, "%", "%%")
	if rest, ok := strings.CutPrefix(p, home+"/"); ok {
		p = "%h/" + rest
	}
	if strings.ContainsAny(p, " \t\"'\\$") {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`)
		p = `"` + r.Replace(p) + `"`
	}
	return p
}

func (d *systemdService) Content(bin, cfg string) []byte {
	return []byte("# " + serviceMarker + ". talaria setup --uninstall removes it.\n" +
		"[Unit]\n" +
		"Description=talaria: runs the Hermes agent's commands on this PC\n" +
		"After=network-online.target " + LegacyUnitName + "\n" +
		"Conflicts=" + LegacyUnitName + "\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=" + d.unitPath(bin) + " run -c " + d.unitPath(cfg) + "\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"OOMPolicy=continue\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n")
}

func (d *systemdService) Load(ctx context.Context) error {
	if _, err := d.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	_, err := d.systemctl(ctx, "enable", UnitName)
	return err
}

func (d *systemdService) LoadStarts() bool { return false }

func (d *systemdService) Active(ctx context.Context) bool {
	out, _ := d.systemctl(ctx, "is-active", UnitName)
	return strings.TrimSpace(string(out)) == stateActive
}

func (d *systemdService) Start(ctx context.Context) error {
	_, err := d.systemctl(ctx, "start", UnitName)
	return err
}

// Restart does not wait for the job, so it also works from a command that
// runs inside the service, such as talaria update run by the agent.
func (d *systemdService) Restart(ctx context.Context) error {
	_, err := d.systemctl(ctx, "--no-block", "restart", UnitName)
	return err
}

func (d *systemdService) Remove(ctx context.Context) error {
	_, err := d.systemctl(ctx, "disable", "--now", UnitName)
	return err
}

func (d *systemdService) Logs() string { return "journalctl --user -u " + UnitName + " -f" }

type launchdService struct{ s *System }

func (l *launchdService) Path() string {
	return filepath.Join(l.s.Home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

func (l *launchdService) Name() string { return "launchd agent " + LaunchdLabel }

func (l *launchdService) target() string { return fmt.Sprintf("gui/%d/%s", l.s.UID, LaunchdLabel) }

func (l *launchdService) launchctl(ctx context.Context, args ...string) ([]byte, error) {
	return l.s.Run.Output(ctx, nil, "launchctl", args...)
}

func (l *launchdService) Available(context.Context) error {
	if _, err := l.s.LookPath("launchctl"); err != nil {
		return ErrNoServiceManager
	}
	return nil
}

// LogFile is where the launchd agent writes the daemon's logs.
func (l *launchdService) LogFile() string {
	return filepath.Join(l.s.Home, "Library", "Logs", "talaria.log")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

func (l *launchdService) Content(bin, cfg string) []byte {
	str := func(v string) string { return "\t\t<string>" + xmlEscape(v) + "</string>\n" }
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ` + serviceMarker + `. talaria setup --uninstall removes it. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + str(bin) + str("run") + str("-c") + str(cfg) + `	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(l.LogFile()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(l.LogFile()) + `</string>
</dict>
</plist>
`)
}

func (l *launchdService) loaded(ctx context.Context) bool {
	_, err := l.launchctl(ctx, "print", l.target())
	return err == nil
}

// Load boots the agent out first when it is loaded, so launchd reads the
// new file, and waits until launchd has let it go: a bootstrap right after
// a bootout can fail. RunAtLoad starts it when it is bootstrapped.
func (l *launchdService) Load(ctx context.Context) error {
	if l.loaded(ctx) {
		if _, err := l.launchctl(ctx, "bootout", l.target()); err != nil {
			return err
		}
		for i := 0; l.loaded(ctx); i++ {
			if i == 50 {
				return fmt.Errorf("launchd still has %s 10 seconds after bootout", l.target())
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if l.disabled(ctx) {
		if _, err := l.launchctl(ctx, "enable", l.target()); err != nil {
			return err
		}
	}
	_, err := l.launchctl(ctx, "bootstrap", fmt.Sprintf("gui/%d", l.s.UID), l.Path())
	return err
}

// disabled reports whether launchd has the agent marked disabled, as
// launchctl disable or an older uninstall leaves it. Only then does Load
// enable it: launchctl enable writes a record to launchd's database that
// stays after the agent is removed, and no command takes it out again.
func (l *launchdService) disabled(ctx context.Context) bool {
	out, err := l.launchctl(ctx, "print-disabled", fmt.Sprintf("gui/%d", l.s.UID))
	if err != nil {
		return true
	}
	for line := range strings.Lines(string(out)) {
		k, v, ok := strings.Cut(line, "=>")
		if !ok || strings.Trim(strings.TrimSpace(k), `"`) != LaunchdLabel {
			continue
		}
		// macOS 13 and later say disabled or enabled, older ones true or false
		v = strings.TrimSpace(v)
		return v == "disabled" || v == "true"
	}
	return false
}

func (l *launchdService) LoadStarts() bool { return true }

func (l *launchdService) Active(ctx context.Context) bool {
	out, err := l.launchctl(ctx, "print", l.target())
	return err == nil && strings.Contains(string(out), "state = running")
}

func (l *launchdService) Start(ctx context.Context) error {
	_, err := l.launchctl(ctx, "kickstart", l.target())
	return err
}

func (l *launchdService) Restart(ctx context.Context) error {
	_, err := l.launchctl(ctx, "kickstart", "-k", l.target())
	return err
}

func (l *launchdService) Remove(ctx context.Context) error {
	if !l.loaded(ctx) {
		return nil
	}
	_, err := l.launchctl(ctx, "bootout", l.target())
	return err
}

func (l *launchdService) Logs() string { return "tail -f " + l.LogFile() }
