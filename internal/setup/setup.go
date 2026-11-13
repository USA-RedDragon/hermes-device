package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BrowserStatus is the daemon's view of the Firefox extension.
type BrowserStatus struct {
	Enabled   bool
	Connected bool
}

// Options configure Setup.
type Options struct {
	// Version is this binary's version.
	Version string
	// Exe is the running binary.
	Exe string
	// HubURL is the hub to pair with and write into a new config file.
	HubURL string
	// ConfigFile is the config file run reads.
	ConfigFile string
	// ConfigExists is true when ConfigFile existed before setup ran, and
	// BrowserEnabled is its browser.enabled then.
	ConfigExists   bool
	BrowserEnabled bool
	// StateDir holds the state file.
	StateDir string
	// BinDir is where the binary goes. Empty means ~/.talaria/bin.
	BinDir string
	// Force replaces a newer binary or a service file setup did not write.
	Force bool
	// OverwriteConfig replaces an existing config file, after a backup.
	OverwriteConfig bool
	// NoService, NoBrowser and NoShell skip those steps.
	NoService, NoBrowser, NoShell bool
	// Wait is how long setup waits for the Firefox extension; 0 skips.
	Wait time.Duration
	// Poll is the interval of BrowserStatus calls. Zero means 2 seconds.
	Poll time.Duration

	// Pair pairs the device unless it holds a token the hub accepts, and
	// reports whether it paired.
	Pair func(ctx context.Context) (bool, error)
	// InstallHost writes Firefox's native messaging host manifest for bin
	// and returns its path, HostManifest. Uninstall removes the manifest
	// only when setup created it.
	InstallHost  func(bin string) (string, error)
	HostManifest string
	// BrowserStatus asks the running daemon about the extension.
	BrowserStatus func(ctx context.Context) (*BrowserStatus, error)
	// Releases finds the signed extension.
	Releases *Releases

	// Shells are the Windows shells this computer can use, the default
	// first; Shell is the one --shell asked for. WSLDistro is the WSL
	// distribution for the wsl shell; empty is WSL's default.
	Shells    []string
	Shell     string
	WSLDistro string
}

// Report is what setup did and what is left for the user.
type Report struct {
	Done []string
	Todo []string
}

func (r *Report) done(format string, args ...any) {
	r.Done = append(r.Done, fmt.Sprintf(format, args...))
}
func (r *Report) todo(format string, args ...any) {
	r.Todo = append(r.Todo, fmt.Sprintf(format, args...))
}

// Print writes the report, and err when the run stopped with one.
func (r *Report) Print(s *System, err error) {
	s.printf("\nDone:\n")
	for _, d := range r.Done {
		s.printf("  - %s\n", d)
	}
	if err != nil {
		s.printf("Failed:\n  - %v\n", err)
	}
	if len(r.Todo) == 0 {
		if err == nil {
			s.printf("Nothing is left to do.\n")
		}
		return
	}
	s.printf("Left to do:\n")
	for _, t := range r.Todo {
		s.printf("  - %s\n", t)
	}
}

// BinPath is where setup installs the binary for o.
func (s *System) BinPath(o *Options) string {
	dir := o.BinDir
	if dir == "" {
		dir = filepath.Join(s.Home, BinDir)
	}
	return filepath.Join(dir, ExeName(s.GOOS))
}

// Setup installs talaria for the user. Every step is recorded in
// the state file as soon as it is done, so a run that fails part way can
// be run again, or undone with Uninstall.
func (s *System) Setup(ctx context.Context, o *Options) (*Report, error) {
	r := &Report{}
	st, err := LoadState(o.StateDir)
	if err != nil {
		return r, err
	}
	save := func() error { return st.Save(o.StateDir) }
	bin := s.BinPath(o)
	changed, err := s.binaryStep(ctx, o, st, r, bin)
	if err != nil {
		return r, err
	}
	if err := s.pairStep(ctx, o, st, r); err != nil {
		return r, err
	}
	ff := s.FindFirefox()
	browser, wrote, err := s.configStep(ctx, o, st, r, ff)
	if err != nil {
		return r, err
	}
	changed = changed || wrote
	if browser {
		if err := s.hostStep(o, st, r, ff, bin); err != nil {
			return r, err
		}
	}
	if !o.NoShell {
		step := s.shellStep
		if s.GOOS == goosWindows {
			step = s.windowsPathStep
		}
		err := step(ctx, st, r, filepath.Dir(bin))
		if serr := save(); err == nil {
			err = serr
		}
		if err != nil {
			return r, err
		}
	}
	running := false
	if o.NoService {
		r.todo("Keep %s run -c %s running (setup ran with --no-service)", bin, o.ConfigFile)
	} else {
		if running, err = s.serviceStep(ctx, o, st, r, bin, changed); err != nil {
			return r, err
		}
		if err := save(); err != nil {
			return r, err
		}
	}
	if browser {
		s.firefoxStep(ctx, o, ff, r, running)
	}
	return r, nil
}

func (s *System) binaryStep(ctx context.Context, o *Options, st *State, r *Report, bin string) (bool, error) {
	_, statErr := os.Lstat(bin)
	wrote, err := s.InstallBinary(ctx, o.Exe, bin, o.Version, o.Force)
	if err == nil && s.GOOS != goosWindows {
		if linked, lerr := LinkAlias(bin); lerr != nil {
			r.todo("Could not link %s to %s: %v", AliasName, bin, lerr)
		} else if linked {
			r.done("Linked %s to %s", filepath.Join(filepath.Dir(bin), AliasName), bin)
		}
	}
	if err != nil || !wrote {
		if err == nil {
			r.done("The binary at %s is up to date", bin)
		}
		return false, err
	}
	if errors.Is(statErr, fs.ErrNotExist) || st.Binary == bin {
		st.Binary = bin
	}
	r.done("Installed the binary at %s", bin)
	return true, st.Save(o.StateDir)
}

func (s *System) pairStep(ctx context.Context, o *Options, st *State, r *Report) error {
	paired, err := o.Pair(ctx)
	if err != nil {
		return fmt.Errorf("pair with %s: %w", o.HubURL, err)
	}
	if !paired {
		r.done("This computer is already paired with %s", o.HubURL)
		return nil
	}
	st.PairedHub = o.HubURL
	r.done("Paired this computer with %s", o.HubURL)
	return st.Save(o.StateDir)
}

// configStep writes the config file when it is missing and returns whether
// browser control is on, and whether it wrote the file.
func (s *System) configStep(ctx context.Context, o *Options, st *State, r *Report, ff *Firefox) (bool, bool, error) {
	if o.ConfigExists && !o.OverwriteConfig {
		r.done("Kept the existing config %s (--overwrite-config replaces it)", o.ConfigFile)
		if o.Shell != "" {
			r.todo("Set device.shell: %s in %s (setup keeps an existing config), then run setup again", o.Shell, o.ConfigFile)
		}
		if !o.BrowserEnabled && !o.NoBrowser && ff.Found() {
			r.todo("Firefox is installed but browser.enabled is off in %s. Set it to true and run setup again to use Firefox", o.ConfigFile)
		}
		return o.BrowserEnabled && !o.NoBrowser, false, nil
	}
	browser := !o.NoBrowser && ff.Found()
	shell := ""
	if s.GOOS == goosWindows {
		var err error
		if shell, err = s.chooseShell(ctx, o); err != nil {
			return false, false, err
		}
	}
	data, err := ConfigYAML(o.HubURL, []string{s.Home}, browser, shell, o.WSLDistro)
	if err != nil {
		return false, false, err
	}
	backup, wrote, err := s.WriteConfig(o.ConfigFile, data, o.OverwriteConfig)
	if err != nil || !wrote {
		return browser, false, err
	}
	st.Config, st.ConfigSHA256 = o.ConfigFile, sha256Hex(data)
	if backup != "" && st.ConfigBackup == "" {
		st.ConfigBackup = backup
	}
	msg := fmt.Sprintf("Wrote %s (commands may run anywhere in %s; browser.enabled %t)", o.ConfigFile, s.Home, browser)
	if shell != "" {
		msg = fmt.Sprintf("Wrote %s (commands run in %s anywhere in %s; browser.enabled %t)", o.ConfigFile, shell, s.Home, browser)
	}
	if backup != "" {
		msg += ", the old file is " + backup
	}
	r.done("%s", msg)
	return browser, true, st.Save(o.StateDir)
}

func (s *System) hostStep(o *Options, st *State, r *Report, ff *Firefox, bin string) error {
	existed := o.HostManifest != "" && exists(o.HostManifest)
	path, err := o.InstallHost(bin)
	if err != nil {
		return fmt.Errorf("install the native messaging host: %w", err)
	}
	if !existed || st.NativeHost == path {
		st.NativeHost = path
	}
	r.done("Registered Firefox's native messaging host in %s", path)
	if ff.Flatpak {
		r.todo("Firefox from Flatpak cannot start the native messaging host outside its sandbox. Install Firefox from your distribution or Mozilla to use it with Talaria")
	}
	return st.Save(o.StateDir)
}

// readLine reads one answer, or returns when ctx ends (Ctrl-C). It reads
// a byte at a time, so that the next prompt gets the next line.
func (s *System) readLine(ctx context.Context) (string, error) {
	type answer struct {
		line string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 1)
		for {
			n, err := s.In.Read(buf)
			if n == 1 {
				b.WriteByte(buf[0])
				if buf[0] == '\n' {
					break
				}
			}
			if err != nil {
				ch <- answer{b.String(), err}
				return
			}
		}
		ch <- answer{b.String(), nil}
	}()
	select {
	case a := <-ch:
		if a.err != nil && a.line == "" {
			return "", nil //nolint:nilerr // no answer is a no
		}
		return a.line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// confirm asks a yes or no question and reports a yes.
func (s *System) confirm(ctx context.Context) (bool, error) {
	s.printf("Make this change? [y/N] ")
	line, err := s.readLine(ctx)
	if err != nil {
		return false, err
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

func (s *System) shellStep(ctx context.Context, st *State, r *Report, binDir string) error {
	rep, err := s.CheckShell(ctx, binDir)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		r.todo("Could not compare your login shell's PATH with bash's: %v", err)
		return nil
	}
	if err := s.binDirStep(ctx, rep, st, r); err != nil {
		return err
	}
	if rep.Shell == "" || len(rep.Missing) == 0 {
		if rep.Shell != "" {
			r.done("bash -l sees the same PATH directories as %s", rep.Shell)
		}
		return nil
	}
	plan := s.PlanShell(rep)
	s.printf("\nYour login shell is %s, but talaria runs agent commands with bash -l, which lacks these PATH directories:\n", rep.Shell)
	for _, d := range rep.Missing {
		s.printf("  %s\n", d)
	}
	s.printf("Commands you run in a terminal may then be \"not found\" for the agent.\n")
	s.printf("Setup can add them to the shared environment file %s and source it from", plan.EnvFile)
	if len(plan.Sources) == 0 {
		s.printf(" (it is already sourced by your startup files)")
	}
	s.printf("%s.\nIt backs up every file it changes first, and setup --uninstall takes the lines out again.\n", listFiles(plan.Sources))
	if !s.Interactive {
		r.todo("Add %s to bash's PATH (run setup in a terminal to have it do so)", strings.Join(rep.Missing, ", "))
		return nil
	}
	yes, err := s.confirm(ctx)
	if err != nil {
		return err
	}
	if !yes {
		r.todo("Add %s to bash's PATH, or agent commands will not find what is in them", strings.Join(rep.Missing, ", "))
		return nil
	}
	if err := s.FixShell(rep, plan, st); err != nil {
		r.todo("Setup could not change the shell files: %v. Add %s to bash's PATH yourself", err, strings.Join(rep.Missing, ", "))
		return nil
	}
	r.done("Added %d PATH directories to %s for bash -l", len(rep.Missing), plan.EnvFile)
	return nil
}

func listFiles(files []string) string {
	var b strings.Builder
	for i, f := range files {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + f)
	}
	return b.String()
}

// binDirStep puts the install directory on PATH for the login shell and
// bash -l, with the same rules as the PATH fix of shellStep.
func (s *System) binDirStep(ctx context.Context, rep *ShellReport, st *State, r *Report) error {
	dir := rep.BinDir
	plan := s.PlanBinDir(rep)
	other := rep.Login != shellBash && rep.Login != shellZsh && rep.Login != shellFish
	var lack []string
	if rep.LoginLacksBin {
		lack = append(lack, rep.Login)
	}
	if rep.BashLacksBin {
		lack = append(lack, "bash -l")
	}
	if len(lack) == 0 {
		if other && !s.OnPath(dir) {
			r.todo("Add %s to PATH in your login shell's startup file, so that the talaria and tal commands are found", dir)
		} else {
			r.done("%s is on PATH", dir)
		}
		return nil
	}
	s.printf("\ntalaria is installed in %s, which is not on PATH for %s, so the talaria and tal commands are \"not found\" there.\n", dir, strings.Join(lack, " and "))
	if plan.EnvFile != "" {
		s.printf("Setup can add it to the shared environment file %s and source it from", plan.EnvFile)
		if len(plan.Sources) == 0 {
			s.printf(" (it is already sourced by your startup files)")
		}
		s.printf("%s", listFiles(plan.Sources))
		if plan.Fish != "" {
			s.printf(", and add it to fish's %s", plan.Fish)
		}
	} else {
		s.printf("Setup can add it to fish's %s", plan.Fish)
	}
	s.printf(".\nIt backs up every file it changes first, and setup --uninstall takes the lines out again.\n")
	if other && !s.OnPath(dir) {
		r.todo("Add %s to PATH in your login shell's startup file, so that the talaria and tal commands are found", dir)
	}
	if !s.Interactive {
		r.todo("Add %s to PATH (run setup in a terminal to have it do so)", dir)
		return nil
	}
	yes, err := s.confirm(ctx)
	if err != nil {
		return err
	}
	if !yes {
		r.todo("Add %s to PATH, or the talaria and tal commands are not found in a terminal", dir)
		return nil
	}
	if err := s.FixBinDir(rep, plan, st); err != nil {
		r.todo("Setup could not change the shell files: %v. Add %s to PATH yourself", err, dir)
		return nil
	}
	r.done("Added %s to PATH in%s", dir, listFiles(plan.Files()))
	return nil
}

func (s *System) serviceStep(ctx context.Context, o *Options, st *State, r *Report, bin string, changed bool) (bool, error) {
	svc, err := s.NewService()
	if err == nil {
		err = svc.Available(ctx)
	}
	if err != nil {
		r.todo("%v. Keep %s run -c %s running another way", err, bin, o.ConfigFile)
		return false, nil
	}
	ours := ""
	if st.Service == svc.Path() {
		ours = st.ServiceSHA256
	}
	res, err := s.WriteServiceFile(svc, bin, o.ConfigFile, ours, o.Force)
	if err != nil {
		return false, err
	}
	if res.Wrote {
		st.Service, st.ServiceSHA256 = svc.Path(), sha256Hex(svc.Content(bin, o.ConfigFile))
		if res.Backup != "" && st.ServiceBackup == "" {
			st.ServiceBackup = res.Backup
		}
		changed = true
		if err := st.Save(o.StateDir); err != nil {
			return false, err
		}
	}
	cfgHash := ""
	if data, err := os.ReadFile(o.ConfigFile); err == nil {
		cfgHash = sha256Hex(data)
	}
	changed = changed || cfgHash != st.RunningConfig
	active := svc.Active(ctx)
	loaded := false
	if res.Wrote || !active {
		if err := svc.Load(ctx); err != nil {
			return false, err
		}
		loaded = true
	}
	switch {
	case loaded && svc.LoadStarts() && svc.Active(ctx):
		st.ServiceOn = st.ServiceOn || !active
		r.done("Started the %s; it starts again at login", svc.Name())
	case !svc.Active(ctx):
		if err := svc.Start(ctx); err != nil {
			return false, err
		}
		st.ServiceOn = true
		r.done("Started the %s; it starts again at login", svc.Name())
	case changed:
		if err := svc.Restart(ctx); err != nil {
			return false, err
		}
		r.done("Restarted the %s", svc.Name())
	default:
		r.done("The %s runs", svc.Name())
	}
	st.RunningConfig = cfgHash
	if res.Adopted {
		r.done("Kept %s, which has the settings setup would write", svc.Path())
	}
	if res.Backup != "" {
		r.done("Replaced %s; the old file is %s", svc.Path(), res.Backup)
	}
	s.printf("Logs: %s\n", svc.Logs())
	return true, nil
}

// xpiURL is the signed extension of this release, or of the latest one.
func (s *System) xpiURL(ctx context.Context, o *Options) (string, error) {
	rel := o.Releases
	if v := Canonical(o.Version); v != "" && !strings.Contains(v, "-") {
		if u, err := rel.XPIURL(ctx, "v"+strings.TrimPrefix(o.Version, "v")); err == nil {
			return u, nil
		}
	}
	tag, err := rel.LatestTag(ctx)
	if err != nil {
		return "", err
	}
	return rel.XPIURL(ctx, tag)
}

func (s *System) firefoxStep(ctx context.Context, o *Options, ff *Firefox, r *Report, running bool) {
	if !ff.ExtensionInstalled() {
		u, err := s.xpiURL(ctx, o)
		switch {
		case err != nil:
			r.todo("Install the Talaria Firefox extension: download talaria-firefox-<version>.xpi from %s/latest, then in Firefox open about:addons, choose the gear menu, \"Install Add-on From File...\" and pick the file (setup could not find it: %v)", o.Releases.BaseURL, err)
		case len(ff.Command) == 0 || !s.HasDesktop():
			r.todo("Open %s in Firefox and choose \"Continue to Installation\", then \"Add\"", u)
		default:
			if err := s.Open(ctx, ff, u); err != nil {
				r.todo("Open %s in Firefox and choose \"Continue to Installation\", then \"Add\" (setup could not start Firefox: %v)", u, err)
			} else {
				s.printf("\nFirefox opens %s.\nChoose \"Continue to Installation\", then \"Add\" to install the Talaria extension.\n", u)
			}
		}
	} else {
		r.done("The Talaria Firefox extension is installed")
	}
	if !running || o.Wait <= 0 || o.BrowserStatus == nil {
		r.todo("Check that the Talaria button in Firefox's toolbar says it is connected to talaria")
		return
	}
	s.waitBrowser(ctx, o, r)
}

func (s *System) waitBrowser(ctx context.Context, o *Options, r *Report) {
	poll := o.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Wait)
	defer cancel()
	s.printf("Waiting up to %s for the Firefox extension (Ctrl-C stops waiting; setup is done otherwise)...\n", o.Wait)
	var last *BrowserStatus
	for {
		st, err := o.BrowserStatus(ctx)
		if err == nil {
			last = st
			switch {
			case !st.Enabled:
				r.todo("The running daemon has browser control off. Check browser.enabled in %s and restart the service", o.ConfigFile)
				return
			case st.Connected:
				r.done("The Firefox extension is connected to talaria")
				return
			}
		}
		select {
		case <-ctx.Done():
			if last == nil {
				r.todo("Setup could not reach the daemon to check Firefox. See the service's logs")
			} else {
				r.todo("The Firefox extension has not connected yet. Install it, or restart Firefox if it is installed")
			}
			return
		case <-time.After(poll):
		}
	}
}

// UninstallOptions configure Uninstall.
type UninstallOptions struct {
	StateDir string
	// RemoveHost removes the native messaging host manifest.
	RemoveHost func() error
	// Unpair deletes the stored device token for hub.
	Unpair func(hub string) error
}

// Uninstall reverses what the state file says setup did. It keeps going
// after an error and keeps the state of what it could not undo.
func (s *System) Uninstall(ctx context.Context, o *UninstallOptions) (*Report, error) {
	r := &Report{}
	st, err := LoadState(o.StateDir)
	if err != nil {
		return r, err
	}
	errs := []error{s.uninstallService(ctx, st, r)}
	if errs[0] == nil {
		errs = append(errs, uninstallHost(o, st, r), uninstallToken(o, st, r), uninstallConfig(st, r),
			uninstallEdits(st, r), s.uninstallUserPath(ctx, st, r), s.uninstallBinary(st, r), s.uninstallCache(r))
	} else {
		r.todo("Kept the binary, the config and the token, because the service still runs. Stop it, then run setup --uninstall again")
	}
	if err := errors.Join(errs...); err != nil {
		return r, errors.Join(err, st.Save(o.StateDir))
	}
	if err := os.Remove(filepath.Join(o.StateDir, StateFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return r, err
	}
	_ = os.Remove(o.StateDir)
	if len(r.Done) == 0 {
		r.done("Setup had not changed anything here")
	}
	return r, nil
}

func (s *System) uninstallService(ctx context.Context, st *State, r *Report) error {
	if st.Service == "" && !st.ServiceOn {
		return nil
	}
	if st.Service != "" {
		data, err := os.ReadFile(st.Service)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			st.Service = ""
		case err != nil:
			return err
		case sha256Hex(data) != st.ServiceSHA256 && !ownService(data):
			r.todo("Left %s alone: it is no longer the file setup wrote", st.Service)
			st.Service, st.ServiceOn = "", false
			return nil
		}
	}
	svc, err := s.NewService()
	if err == nil {
		err = svc.Remove(ctx)
	}
	if err != nil {
		return fmt.Errorf("stop the service: %w", err)
	}
	st.ServiceOn = false
	r.done("Stopped the service and disabled it at login")
	if st.Service == "" {
		return nil
	}
	data, _ := os.ReadFile(st.Service)
	if sha256Hex(data) != st.ServiceSHA256 {
		backup, err := s.backup(st.Service)
		if err != nil {
			return err
		}
		r.done("You had edited %s; the edited copy is %s", st.Service, backup)
	}
	if err := os.Remove(st.Service); err != nil {
		return err
	}
	r.done("Removed %s", st.Service)
	if l, ok := svc.(*launchdService); ok {
		// launchd wrote the daemon's log where the agent file said
		if err := os.Remove(l.LogFile()); err == nil {
			r.done("Removed the service's log %s", l.LogFile())
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if st.ServiceBackup != "" {
		if err := os.Rename(st.ServiceBackup, st.Service); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		} else if err == nil {
			r.done("Put back your earlier %s", st.Service)
			r.todo("Your earlier service file is back in place but not enabled. Enable it if you still use it")
		}
	}
	st.Service, st.ServiceSHA256, st.ServiceBackup = "", "", ""
	if s.GOOS == "linux" {
		_, _ = s.Run.Output(ctx, nil, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

func uninstallHost(o *UninstallOptions, st *State, r *Report) error {
	if st.NativeHost == "" {
		return nil
	}
	if err := o.RemoveHost(); err != nil {
		return fmt.Errorf("remove the native messaging host: %w", err)
	}
	r.done("Removed Firefox's native messaging host")
	r.todo("Remove the Talaria extension in Firefox: about:addons, Talaria, Remove")
	st.NativeHost = ""
	return nil
}

func uninstallToken(o *UninstallOptions, st *State, r *Report) error {
	if st.PairedHub == "" {
		return nil
	}
	if err := o.Unpair(st.PairedHub); err != nil {
		return fmt.Errorf("delete the device token: %w", err)
	}
	r.done("Deleted the device token for %s", st.PairedHub)
	r.todo("Remove this computer on the hub (Settings, Computers) so its old token stops working there too")
	st.PairedHub = ""
	return nil
}

func uninstallConfig(st *State, r *Report) error {
	if st.Config == "" {
		return nil
	}
	data, err := os.ReadFile(st.Config)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case sha256Hex(data) != st.ConfigSHA256:
		r.todo("Kept %s: it changed since setup wrote it. Delete it yourself if you do not need it", st.Config)
	default:
		if err := os.Remove(st.Config); err != nil {
			return err
		}
		r.done("Removed %s", st.Config)
		if st.ConfigBackup != "" && os.Rename(st.ConfigBackup, st.Config) == nil {
			r.done("Put back your earlier %s", st.Config)
		}
	}
	if st.ConfigBackup != "" && exists(st.ConfigBackup) {
		r.done("Kept the backup %s", st.ConfigBackup)
	}
	st.Config, st.ConfigSHA256, st.ConfigBackup = "", "", ""
	return nil
}

func uninstallEdits(st *State, r *Report) error {
	var kept []Edit
	var errs []error
	for _, e := range st.Edits {
		changed, err := RemoveBlocks(e.File, e.Created)
		if err != nil {
			errs = append(errs, err)
			kept = append(kept, e)
			continue
		}
		if changed {
			r.done("Took the setup lines out of %s", e.File)
		}
		if e.Backup != "" {
			r.done("Kept the backup %s", e.Backup)
		}
	}
	st.Edits = kept
	return errors.Join(errs...)
}

// uninstallCache removes talaria's cache directory: the release lookup
// cache, and on Linux the managed Hermes builds of talaria chat.
func (s *System) uninstallCache(r *Report) error {
	if s.CacheDir == "" {
		return nil
	}
	dir := filepath.Join(s.CacheDir, dirName)
	if s.CacheDir == s.ConfigDir {
		// Windows keeps the config, the token and setup's state there too
		// (userdir.Config), so only the release lookup cache goes.
		f := filepath.Join(dir, "release.json")
		if err := os.Remove(f); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		r.done("Removed the cache %s", f)
		return nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	r.done("Removed the cache %s", dir)
	return nil
}

func (s *System) uninstallBinary(st *State, r *Report) error {
	if st.Binary == "" {
		return nil
	}
	aside, err := removeBinary(st.Binary)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if aside != "" {
		r.done("Moved %s to %s, because it still runs and Windows cannot delete a running program. Delete that file after this command ends", st.Binary, aside)
	} else {
		r.done("Removed %s", st.Binary)
	}
	alias := filepath.Join(filepath.Dir(st.Binary), AliasName)
	if target, err := os.Readlink(alias); err == nil && (target == filepath.Base(st.Binary) || target == st.Binary) {
		_ = os.Remove(alias)
	}
	if dir := filepath.Join(s.Home, BinDir); filepath.Dir(st.Binary) == dir && os.Remove(dir) == nil {
		_ = os.Remove(filepath.Dir(dir))
	}
	st.Binary = ""
	return nil
}
