package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/USA-RedDragon/talaria/internal/filelock"
)

// Legacy names of the native messaging host, the config directory and the
// cache directory, from releases before the rename to talaria.
const (
	legacyDirName  = "hermes-device"
	dirName        = "talaria"
	legacyHostName = "hermes_device"
	hostName       = "talaria"
)

// MigrateOptions configure Migrate.
type MigrateOptions struct {
	// UnderLegacyService is true when the legacy service runs this
	// process, so that a hand-over to the talaria service stops it.
	UnderLegacyService bool
	// KeepService leaves the legacy service as it is, for a command that
	// runs while the legacy daemon may run work: the daemon moves its
	// service when it restarts.
	KeepService bool
}

// Migration is what Migrate did.
type Migration struct {
	// Moved lists each move as "old -> new".
	Moved []string
	// Notes are other changes and what Migrate left alone, and why.
	Notes []string
	// HandOff is true when Migrate asked the service manager to start the
	// talaria service in place of the legacy service that runs this
	// process. The process should then stop without doing its work.
	HandOff bool
}

func (m *Migration) moved(from, to string) { m.Moved = append(m.Moved, from+" -> "+to) }

func (m *Migration) note(format string, args ...any) {
	m.Notes = append(m.Notes, fmt.Sprintf(format, args...))
}

// Empty reports whether Migrate found nothing to do.
func (m *Migration) Empty() bool { return len(m.Moved) == 0 && len(m.Notes) == 0 && !m.HandOff }

type migratePaths struct {
	legacyBin, bin            string
	legacyConfig, config      string
	legacyCache, cache        string
	legacyManifest, manifest  string
	legacyService, newService string
}

func (s *System) migratePaths() migratePaths {
	p := migratePaths{
		legacyBin:    filepath.Join(s.Home, LegacyBinDir, LegacyBinaryName),
		bin:          filepath.Join(s.Home, BinDir, BinaryName),
		legacyConfig: filepath.Join(s.ConfigDir, legacyDirName),
		config:       filepath.Join(s.ConfigDir, dirName),
		legacyCache:  filepath.Join(s.CacheDir, legacyDirName),
		cache:        filepath.Join(s.CacheDir, dirName),
	}
	dir := s.manifestDir()
	p.legacyManifest = filepath.Join(dir, legacyHostName+".json")
	p.manifest = filepath.Join(dir, hostName+".json")
	if s.GOOS == darwin {
		agents := filepath.Join(s.Home, "Library", "LaunchAgents")
		p.legacyService = filepath.Join(agents, LegacyLaunchdLabel+".plist")
		p.newService = filepath.Join(agents, LaunchdLabel+".plist")
	} else {
		d := (&systemdService{s: s}).dir()
		p.legacyService = filepath.Join(d, LegacyUnitName)
		p.newService = filepath.Join(d, UnitName)
	}
	return p
}

func (s *System) manifestDir() string {
	if s.GOOS == darwin {
		return filepath.Join(s.Home, "Library", "Application Support", "Mozilla", "NativeMessagingHosts")
	}
	return filepath.Join(s.Home, ".mozilla", "native-messaging-hosts")
}

// NeedsMigration reports whether a file of a release before the rename is
// still in place.
func (s *System) NeedsMigration() bool {
	p := s.migratePaths()
	if isRegular(p.legacyBin) || isDir(p.legacyConfig) || isDir(p.legacyCache) || isRegular(p.legacyService) {
		return true
	}
	m, err := readManifest(p.legacyManifest)
	return err == nil && m.Path != p.bin && (m.Path == p.legacyBin || fileExists(p.bin))
}

// MigratedConfigPath returns path moved into the talaria config directory
// when it is in the legacy one, else path.
func (s *System) MigratedConfigPath(path string) string {
	p := s.migratePaths()
	if rest, ok := strings.CutPrefix(path, p.legacyConfig+string(filepath.Separator)); ok {
		return filepath.Join(p.config, rest)
	}
	return path
}

// Migrate moves what a release before the rename to talaria installed to
// the talaria names: the binary (leaving a hermes-device link to it and
// linking tal to it), the config and cache directories, setup's record,
// Firefox's native messaging host and the service. It keeps the legacy
// host manifest, pointed at the talaria binary, for extension versions
// that still use it. A step that fails is noted and the others still run.
func (s *System) Migrate(ctx context.Context, o MigrateOptions) (*Migration, error) {
	m := &Migration{}
	if !s.NeedsMigration() && !o.UnderLegacyService {
		return m, nil
	}
	unlock, err := s.migrateLock()
	if err != nil {
		return m, err
	}
	defer unlock()
	p := s.migratePaths()
	var errs []error
	if err := s.migrateBinary(p, m); err != nil {
		errs = append(errs, fmt.Errorf("binary: %w", err))
	}
	if err := moveDir(p.legacyConfig, p.config, m); err != nil {
		errs = append(errs, fmt.Errorf("config directory: %w", err))
	}
	if err := moveDir(p.legacyCache, p.cache, m); err != nil {
		errs = append(errs, fmt.Errorf("cache directory: %w", err))
	}
	if err := s.migrateConfigFile(p, m); err != nil {
		errs = append(errs, fmt.Errorf("config file: %w", err))
	}
	if err := s.migrateHost(p, m); err != nil {
		errs = append(errs, fmt.Errorf("native messaging host: %w", err))
	}
	shas, activate, err := s.migrateService(ctx, p, o, m)
	if err != nil {
		errs = append(errs, fmt.Errorf("service: %w", err))
	}
	if err := s.migrateState(p, shas, m); err != nil {
		errs = append(errs, fmt.Errorf("setup record: %w", err))
	}
	if activate != nil {
		if err := activate(); err != nil {
			errs = append(errs, fmt.Errorf("service: %w", err))
		}
	}
	return m, errors.Join(errs...)
}

func (s *System) migrateLock() (func(), error) {
	dir := filepath.Join(s.Home, filepath.Dir(BinDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".migrate.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := filelock.Lock(f, filelock.Exclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = filelock.Unlock(f)
		_ = f.Close()
	}, nil
}

func isRegular(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// migrateBinary moves the legacy binary to the talaria path, replaces it
// with a link there and links tal. A talaria binary that is already in
// place wins.
func (s *System) migrateBinary(p migratePaths, m *Migration) error {
	if isRegular(p.legacyBin) {
		if err := os.MkdirAll(filepath.Dir(p.bin), 0o755); err != nil {
			return err
		}
		if fileExists(p.bin) {
			m.note("replaced %s with a link: %s is already installed", p.legacyBin, p.bin)
		} else {
			if err := linkOrCopy(p.legacyBin, p.bin); err != nil {
				return err
			}
			m.moved(p.legacyBin, p.bin)
			// Migrate runs without a terminal to ask in, and setup changes
			// shell files only with a yes, so it only says what to run.
			if hint := s.PathHint(p.bin); hint != "" {
				m.note("%s", hint)
			}
		}
		if err := replaceWithLink(p.legacyBin, p.bin); err != nil {
			return err
		}
		m.note("linked %s to %s for commands and editors that still use the old name", p.legacyBin, p.bin)
	}
	if fileExists(p.bin) {
		linked, err := LinkAlias(p.bin)
		if err != nil {
			return err
		}
		if linked {
			m.note("linked %s to %s", filepath.Join(filepath.Dir(p.bin), AliasName), p.bin)
		}
	}
	return nil
}

// linkOrCopy makes to a hard link of from, or a copy across file systems,
// so that from stays in place until replaceWithLink swaps it.
func linkOrCopy(from, to string) error {
	err := os.Link(from, to)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	return copyFile(from, to)
}

// replaceWithLink atomically replaces path with a symbolic link to target.
func replaceWithLink(path, target string) error {
	tmp := path + ".talaria-link"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// moveFile renames from to to, or copies and removes it across file
// systems.
func moveFile(from, to string) error {
	err := os.Rename(from, to)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFile(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}

// copyFile copies the regular file from to to, with its mode, through a
// temporary file.
func copyFile(from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(to), "."+filepath.Base(to)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), to)
}

// moveDir moves the directory from to to. When to exists, it moves each
// entry that to lacks and leaves the others, and removes from when it is
// then empty. Sockets are not carried across file systems.
func moveDir(from, to string, m *Migration) error {
	if !isDir(from) {
		return nil
	}
	if !fileExists(to) {
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		err := os.Rename(from, to)
		if err == nil {
			m.moved(from, to)
			return nil
		}
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := os.Mkdir(to, 0o700); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	var kept []string
	for _, e := range entries {
		src, dst := filepath.Join(from, e.Name()), filepath.Join(to, e.Name())
		if fileExists(dst) {
			kept = append(kept, e.Name())
			continue
		}
		err := os.Rename(src, dst)
		if errors.Is(err, syscall.EXDEV) {
			switch {
			case e.Type().IsRegular():
				err = moveFile(src, dst)
			case e.Type()&fs.ModeSocket != 0:
				err = os.Remove(src)
			case e.IsDir():
				err = moveDir(src, dst, &Migration{})
			default:
				kept = append(kept, e.Name())
				continue
			}
		}
		if err != nil {
			return err
		}
	}
	if len(kept) > 0 {
		m.note("left %s in %s: %s has files with the same names", strings.Join(kept, ", "), from, to)
		return nil
	}
	if err := os.Remove(from); err != nil {
		return err
	}
	m.moved(from, to)
	return nil
}

// rewriteConfigPaths points every path in text that names the legacy
// config directory (as an absolute path or with systemd's %h) at the
// talaria one.
func (s *System) rewriteConfigPaths(p migratePaths, text string) string {
	pairs := [][2]string{{p.legacyConfig, p.config}}
	if rel, err := filepath.Rel(s.Home, p.legacyConfig); err == nil && !strings.HasPrefix(rel, "..") {
		newRel, _ := filepath.Rel(s.Home, p.config)
		pairs = append(pairs, [2]string{"%h/" + rel, "%h/" + newRel})
	}
	for _, pr := range pairs {
		re := regexp.MustCompile(regexp.QuoteMeta(pr[0]) + `(/|$|[\s"'=:,;\]}<])`)
		text = re.ReplaceAllString(text, strings.ReplaceAll(pr[1], "$", "$$")+"${1}")
	}
	return text
}

// migrateConfigFile points paths in the moved config file that named the
// legacy config directory at the talaria one.
func (s *System) migrateConfigFile(p migratePaths, m *Migration) error {
	cfg := filepath.Join(p.config, "config.yaml")
	data, err := os.ReadFile(cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	out := s.rewriteConfigPaths(p, string(data))
	if out == string(data) {
		return nil
	}
	fi, err := os.Stat(cfg)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(cfg, []byte(out), fi.Mode().Perm()); err != nil {
		return err
	}
	m.note("changed paths in %s from %s to %s", cfg, p.legacyConfig, p.config)
	return nil
}

type hostManifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedExtensions []string `json:"allowed_extensions"`
}

func readManifest(path string) (*hostManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hm hostManifest
	if err := json.Unmarshal(data, &hm); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &hm, nil
}

func writeManifest(path string, hm *hostManifest) error {
	data, err := json.MarshalIndent(hm, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}

// migrateHost writes the talaria host manifest and its socket file from
// the legacy ones, and points the legacy ones at the talaria binary and
// socket, so that both extension versions connect.
func (s *System) migrateHost(p migratePaths, m *Migration) error {
	old, err := readManifest(p.legacyManifest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(old.AllowedExtensions, ExtensionID) || old.Path == p.bin || !fileExists(p.bin) {
		return nil
	}
	dir := filepath.Dir(p.legacyManifest)
	legacySock := filepath.Join(dir, legacyHostName+".socket")
	sock := ""
	if data, err := os.ReadFile(legacySock); err == nil {
		sock = s.MigratedConfigPath(strings.TrimSpace(string(data)))
	}
	if !fileExists(p.manifest) {
		hm := &hostManifest{Name: hostName, Description: "talaria browser control", Path: p.bin, Type: "stdio",
			AllowedExtensions: []string{ExtensionID}}
		if err := writeManifest(p.manifest, hm); err != nil {
			return err
		}
		if sock != "" {
			if err := writeFileAtomic(filepath.Join(dir, hostName+".socket"), []byte(sock+"\n"), 0o644); err != nil {
				return err
			}
		}
		m.note("registered %s as Firefox's native messaging host %s", p.bin, hostName)
	}
	old.Path = p.bin
	if err := writeManifest(p.legacyManifest, old); err != nil {
		return err
	}
	if sock != "" {
		if err := writeFileAtomic(legacySock, []byte(sock+"\n"), 0o644); err != nil {
			return err
		}
	}
	m.note("pointed the legacy native messaging host %s at %s, for extension versions before the rename", p.legacyManifest, p.bin)
	return nil
}

// migrateServiceContent renames the paths, names and label in a legacy
// unit or launchd agent file, and keeps every other setting. A file that
// setup did not write gets migratedServiceMarker, so that setup treats the
// result as its own. A unit
// without an OOMPolicy gets OOMPolicy=continue, so that an agent command
// the kernel kills for memory does not stop the service.
func (s *System) migrateServiceContent(p migratePaths, data string) string {
	r := []string{
		filepath.Join(s.Home, LegacyBinDir, LegacyBinaryName), p.bin,
		"%h/" + LegacyBinDir + "/" + LegacyBinaryName, "%h/" + BinDir + "/" + BinaryName,
		legacyServiceMarker + ". hermes-device setup --uninstall", serviceMarker + ". talaria setup --uninstall",
		"Description=hermes-device:", "Description=talaria:",
		"<string>" + LegacyLaunchdLabel + "</string>", "<string>" + LaunchdLabel + "</string>",
		filepath.Join("Library", "Logs", "hermes-device.log"), filepath.Join("Library", "Logs", "talaria.log"),
	}
	out := s.rewriteConfigPaths(p, strings.NewReplacer(r...).Replace(data))
	own := strings.Contains(out, serviceMarker)
	if s.GOOS == darwin {
		if i := strings.Index(out, "<plist"); i >= 0 && !own {
			out = out[:i] + "<!-- " + migratedServiceMarker + ". talaria setup replaces it with its own file. -->\n" + out[i:]
		}
		return out
	}
	if !own {
		out = "# " + migratedServiceMarker + ". talaria setup replaces it with its own unit.\n" + out
	}
	if !strings.Contains(out, "\nOOMPolicy=") {
		if i := strings.Index(out, "[Service]\n"); i >= 0 {
			out = out[:i+len("[Service]\n")] + "OOMPolicy=continue\n" + out[i+len("[Service]\n"):]
		}
	}
	extra := "After=" + LegacyUnitName + "\nConflicts=" + LegacyUnitName + "\n"
	if i := strings.Index(out, "[Unit]\n"); i >= 0 {
		return out[:i+len("[Unit]\n")] + extra + out[i+len("[Unit]\n"):]
	}
	return "[Unit]\n" + extra + "\n" + out
}

// serviceSHAs are the hashes of the legacy service file and of the talaria
// one that migrateService wrote from it.
type serviceSHAs struct{ legacy, written string }

// migrateService writes the talaria service file from the legacy one and
// returns the hashes of the files when it wrote one, and the step that
// replaces the legacy service with it in the service manager. That step
// runs last: when the legacy service runs this process, the service
// manager stops the process during it. With systemd, the talaria unit
// conflicts with the legacy one, so starting it stops the legacy one
// first.
func (s *System) migrateService(ctx context.Context, p migratePaths, o MigrateOptions, m *Migration) (serviceSHAs, func() error, error) {
	var shas serviceSHAs
	svc, err := s.NewService()
	if err != nil {
		return shas, nil, nil //nolint:nilerr // no service manager here: nothing to move
	}
	data, rerr := os.ReadFile(p.legacyService)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return shas, nil, rerr
	}
	if rerr != nil && !o.UnderLegacyService || o.KeepService && !o.UnderLegacyService {
		return shas, nil, nil
	}
	if err := svc.Available(ctx); err != nil {
		if rerr == nil {
			m.note("left %s: %v", p.legacyService, err)
		}
		return shas, nil, nil
	}
	if rerr == nil && !fileExists(p.newService) {
		out := []byte(s.migrateServiceContent(p, string(data)))
		if err := os.MkdirAll(filepath.Dir(p.newService), 0o755); err != nil {
			return shas, nil, err
		}
		if err := writeFileAtomic(p.newService, out, 0o644); err != nil {
			return shas, nil, err
		}
		shas = serviceSHAs{legacy: sha256Hex(data), written: sha256Hex(out)}
	}
	if s.GOOS == darwin {
		return shas, func() error { return s.migrateLaunchd(ctx, p, o, m, rerr == nil) }, nil
	}
	return shas, func() error { return s.migrateSystemd(ctx, p, o, m, rerr == nil) }, nil
}

func (s *System) systemctl(ctx context.Context, args ...string) (string, error) {
	out, err := s.Run.Output(ctx, nil, "systemctl", append([]string{"--user"}, args...)...)
	return strings.TrimSpace(string(out)), err
}

func (s *System) migrateSystemd(ctx context.Context, p migratePaths, o MigrateOptions, m *Migration, haveLegacy bool) error {
	enabled, active := false, o.UnderLegacyService
	if haveLegacy {
		st, _ := s.systemctl(ctx, "is-enabled", LegacyUnitName)
		enabled = st != "" && st != "disabled" && st != "masked" && st != "static" && st != "not-found"
		if st, _ := s.systemctl(ctx, "is-active", LegacyUnitName); st == stateActive || st == "activating" || st == "reloading" {
			active = true
		}
		// Drop-ins go first, so the talaria unit starts with them.
		control := filepath.Join(filepath.Dir(filepath.Dir(p.legacyService)), "user.control")
		for _, d := range [][2]string{
			{p.legacyService + ".d", p.newService + ".d"},
			{filepath.Join(control, LegacyUnitName+".d"), filepath.Join(control, UnitName+".d")},
		} {
			if err := s.moveDropIns(p, d[0], d[1], m); err != nil {
				return err
			}
		}
	}
	if !fileExists(p.newService) {
		return nil
	}
	if _, err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if enabled {
		if _, err := s.systemctl(ctx, "enable", UnitName); err != nil {
			return err
		}
		m.note("enabled %s in place of %s", UnitName, LegacyUnitName)
	}
	if active {
		// The talaria unit conflicts with the legacy one, so the service
		// manager stops the legacy one before it starts the talaria one.
		// When the legacy one runs this process, the stop ends systemctl
		// too, after it queued the start, and this process goes on until
		// it exits.
		if _, err := s.systemctl(ctx, "--no-block", "start", UnitName); err != nil && (!o.UnderLegacyService || !killedBySignal(err)) {
			return err
		}
		m.note("started %s, which stops %s", UnitName, LegacyUnitName)
		m.HandOff = o.UnderLegacyService
	}
	if !haveLegacy {
		return nil
	}
	if enabled {
		if _, err := s.systemctl(ctx, "disable", LegacyUnitName); err != nil && !killedBySignal(err) {
			return err
		}
	}
	if err := os.Remove(p.legacyService); err != nil {
		return err
	}
	m.moved(p.legacyService, p.newService)
	if _, err := s.systemctl(ctx, "daemon-reload"); err != nil && !killedBySignal(err) {
		return err
	}
	return nil
}

// moveDropIns moves a legacy drop-in directory and points the paths in its
// files at the talaria config directory.
func (s *System) moveDropIns(p migratePaths, from, to string, m *Migration) error {
	if !isDir(from) {
		return nil
	}
	if err := moveDir(from, to, m); err != nil {
		return err
	}
	entries, err := os.ReadDir(to)
	if err != nil {
		return err
	}
	for _, e := range entries {
		f := filepath.Join(to, e.Name())
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if out := s.migrateServiceContentDropIn(p, string(data)); out != string(data) {
			if err := writeFileAtomic(f, []byte(out), 0o644); err != nil {
				return err
			}
			m.note("changed paths in %s", f)
		}
	}
	return nil
}

// migrateServiceContentDropIn is migrateServiceContent without the lines
// that only the unit itself gets.
func (s *System) migrateServiceContentDropIn(p migratePaths, data string) string {
	r := strings.NewReplacer(
		filepath.Join(s.Home, LegacyBinDir, LegacyBinaryName), p.bin,
		"%h/"+LegacyBinDir+"/"+LegacyBinaryName, "%h/"+BinDir+"/"+BinaryName,
	)
	return s.rewriteConfigPaths(p, r.Replace(data))
}

func (s *System) launchctl(ctx context.Context, args ...string) error {
	_, err := s.Run.Output(ctx, nil, "launchctl", args...)
	return err
}

func (s *System) migrateLaunchd(ctx context.Context, p migratePaths, o MigrateOptions, m *Migration, haveLegacy bool) error {
	domain := fmt.Sprintf("gui/%d", s.UID)
	legacy := domain + "/" + LegacyLaunchdLabel
	loaded := s.launchctl(ctx, "print", legacy) == nil
	if haveLegacy {
		if err := os.Remove(p.legacyService); err != nil {
			return err
		}
		m.moved(p.legacyService, p.newService)
	}
	if !loaded || !fileExists(p.newService) {
		return nil
	}
	if o.UnderLegacyService {
		// Booting the legacy agent out ends this process, so the talaria
		// agent goes in first. Its own migration boots the legacy agent
		// out when this process has not done it yet.
		if s.launchctl(ctx, "print", domain+"/"+LaunchdLabel) != nil {
			if err := s.launchctl(ctx, "bootstrap", domain, p.newService); err != nil {
				return err
			}
		}
		m.note("loaded %s and booted out %s", LaunchdLabel, LegacyLaunchdLabel)
		m.HandOff = true
		return s.launchctl(ctx, "bootout", legacy)
	}
	if err := s.launchctl(ctx, "bootout", legacy); err != nil {
		return err
	}
	if s.launchctl(ctx, "print", domain+"/"+LaunchdLabel) == nil {
		m.note("booted out %s", LegacyLaunchdLabel)
		return nil
	}
	m.note("booted out %s and loaded %s", LegacyLaunchdLabel, LaunchdLabel)
	return s.launchctl(ctx, "bootstrap", domain, p.newService)
}

// migrateState points setup's record at the moved files.
func (s *System) migrateState(p migratePaths, shas serviceSHAs, m *Migration) error {
	if !isRegular(filepath.Join(p.config, StateFileName)) {
		return nil
	}
	st, err := LoadState(p.config)
	if err != nil {
		return err
	}
	changed := false
	set := func(field *string, v string) {
		if *field != v {
			*field, changed = v, true
		}
	}
	if st.Binary == p.legacyBin {
		set(&st.Binary, p.bin)
	}
	set(&st.Config, s.MigratedConfigPath(st.Config))
	if st.Service == p.legacyService {
		set(&st.Service, p.newService)
		if shas.written != "" && st.ServiceSHA256 == shas.legacy {
			set(&st.ServiceSHA256, shas.written)
		}
	}
	if st.NativeHost == p.legacyManifest {
		set(&st.NativeHost, p.manifest)
	}
	if !changed {
		return nil
	}
	if err := st.Save(p.config); err != nil {
		return err
	}
	m.note("updated the paths in %s", filepath.Join(p.config, StateFileName))
	return nil
}

// killedBySignal reports whether err is a program that a signal ended. The
// service manager ends systemctl with the legacy service it stops, after
// systemctl queued the start of the talaria service.
func killedBySignal(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// LegacyServiceActive reports whether the service of a release before the
// rename runs. A command that is not that service leaves the migration to
// it: moving its service would stop work it runs.
func (s *System) LegacyServiceActive(ctx context.Context) bool {
	if s.GOOS == darwin {
		return s.launchctl(ctx, "print", fmt.Sprintf("gui/%d/%s", s.UID, LegacyLaunchdLabel)) == nil
	}
	if _, err := s.LookPath("systemctl"); err != nil {
		return false
	}
	st, _ := s.systemctl(ctx, "is-active", LegacyUnitName)
	return st == stateActive || st == "activating" || st == "reloading"
}
