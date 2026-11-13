package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Markers around every block setup adds to a shell file.
const (
	blockBegin = "# >>> talaria setup >>>"
	blockEnd   = "# <<< talaria setup <<<"
	pathMarker = "__TALARIA_PATH__="
)

// Markers of the blocks that releases before the rename to talaria wrote.
// Setup leaves such a block in place, counts it as the same block, and
// removes it on uninstall.
const (
	legacyBlockBegin = "# >>> hermes-device setup >>>"
	legacyBlockEnd   = "# <<< hermes-device setup <<<"
)

// pathVar is the loop variable of the PATH block.
const (
	pathVar       = "talaria_dir"
	legacyPathVar = "hermes_device_dir"
)

// legacyBlock is block as a release before the rename wrote it.
func legacyBlock(block string) string {
	return strings.NewReplacer(blockBegin, legacyBlockBegin, blockEnd, legacyBlockEnd, pathVar, legacyPathVar,
		"talaria runs agent commands", "hermes-device runs agent commands").Replace(block)
}

// basePath is the PATH that the service manager gives the daemon, before
// bash -l adds to it.
const basePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

const shellTimeout = 15 * time.Second

// Login shells that setup checks.
const (
	shellBash = "bash"
	shellZsh  = "zsh"
	shellFish = "fish"
)

// ShellReport compares the login shell's PATH with what agent commands get.
type ShellReport struct {
	// Shell is the login shell's base name: zsh or fish. Empty when the
	// login shell is bash or another shell that setup does not check.
	Shell string
	// Missing are directories on the login shell's PATH that bash -l does
	// not have, in the login shell's order. It never holds BinDir.
	Missing []string
	// Login is the login shell's base name, also when it is bash or a
	// shell setup does not check.
	Login string
	// BinDir is the install directory CheckShell looked for.
	// LoginLacksBin is true when the login shell is bash, zsh or fish and
	// its PATH lacks BinDir; BashLacksBin when bash -l's PATH lacks it.
	BinDir                      string
	LoginLacksBin, BashLacksBin bool
}

// shellEnv is a small environment both shells start from, as under the
// service manager.
func (s *System) shellEnv() []string {
	env := []string{"HOME=" + s.Home, "PATH=" + basePath, "TERM=dumb"}
	for _, k := range []string{"USER", "LOGNAME", "LANG", "XDG_RUNTIME_DIR", "ZDOTDIR"} {
		if v := s.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func parsePath(out []byte) []string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if _, v, ok := strings.Cut(line, pathMarker); ok {
			return strings.Split(strings.TrimSpace(v), ":")
		}
	}
	return nil
}

func printPath(name string) string {
	if name == shellFish {
		return `printf '\n` + pathMarker + `%s\n' (string join : $PATH)`
	}
	return `printf '\n` + pathMarker + `%s\n' "$PATH"`
}

func onPath(path []string, dir string) bool {
	dir = filepath.Clean(dir)
	return slices.ContainsFunc(path, func(d string) bool { return d != "" && filepath.Clean(d) == dir })
}

// CheckShell runs the login shell (interactive, as in a terminal) and bash
// -l (as agent commands run), lists the directories bash lacks, and checks
// that both have binDir on PATH. An empty binDir skips that check.
func (s *System) CheckShell(ctx context.Context, binDir string) (*ShellReport, error) {
	name := filepath.Base(s.Shell)
	r := &ShellReport{BinDir: binDir}
	if s.Shell != "" {
		r.Login = name
	}
	switch name {
	case shellZsh, shellFish, shellBash:
	default:
		if binDir == "" {
			return r, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	var login []string
	if r.Login == shellZsh || r.Login == shellFish || r.Login == shellBash && binDir != "" {
		out, err := s.Run.Output(ctx, s.shellEnv(), s.Shell, "-l", "-i", "-c", printPath(name))
		if login = parsePath(out); login == nil {
			return nil, fmt.Errorf("read PATH from %s: %w", s.Shell, err)
		}
	}
	if login == nil && binDir == "" {
		return r, nil
	}
	out, err := s.Run.Output(ctx, s.shellEnv(), shellBash, "-l", "-c", printPath(shellBash))
	bash := parsePath(out)
	if bash == nil {
		return nil, fmt.Errorf("read PATH from bash -l: %w", err)
	}
	if binDir != "" {
		r.BashLacksBin = !onPath(bash, binDir)
		r.LoginLacksBin = login != nil && !onPath(login, binDir)
	}
	if name != shellZsh && name != shellFish {
		return r, nil
	}
	r.Shell = name
	for _, d := range login {
		if d == "" || !filepath.IsAbs(d) || slices.Contains(bash, d) || slices.Contains(r.Missing, d) ||
			binDir != "" && filepath.Clean(d) == filepath.Clean(binDir) {
			continue
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			r.Missing = append(r.Missing, d)
		}
	}
	return r, nil
}

// PathHint is one line that tells the user to run bin's setup to put
// bin's directory on PATH, or "" when this process's PATH has it.
func (s *System) PathHint(bin string) string {
	dir := filepath.Dir(bin)
	if s.OnPath(dir) {
		return ""
	}
	tilde := func(p string) string {
		if rest, ok := strings.CutPrefix(p, s.Home+"/"); ok {
			return "~/" + rest
		}
		return p
	}
	return fmt.Sprintf("%s is not on PATH, so the talaria and tal commands are \"not found\" in a terminal. Run %s setup to add it.", tilde(dir), tilde(bin))
}

// OnPath reports whether dir is on this process's PATH.
func (s *System) OnPath(dir string) bool {
	return onPath(strings.Split(s.Getenv("PATH"), ":"), dir)
}

// EnvFile is the shared environment file, as ~/.config/shell/env.sh.
func (s *System) EnvFile() string {
	return filepath.Join(s.ConfigDir, "shell", "env.sh")
}

// BashStartup is the file bash -l reads: the first of ~/.bash_profile,
// ~/.bash_login and ~/.profile that exists, else ~/.bash_profile.
func (s *System) BashStartup() string {
	for _, n := range []string{".bash_profile", ".bash_login", ".profile"} {
		p := filepath.Join(s.Home, n)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(s.Home, ".bash_profile")
}

// BashRC is ~/.bashrc, which interactive bash shells that are not login
// shells read, as most terminals on Linux start.
func (s *System) BashRC() string { return filepath.Join(s.Home, ".bashrc") }

// FishConf is the file fish reads at start that setup writes to.
func (s *System) FishConf() string {
	base := s.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(s.Home, ".config")
	}
	return filepath.Join(base, "fish", "conf.d", "talaria.fish")
}

// ZshStartup is ~/.zshrc, under ZDOTDIR when that is set.
func (s *System) ZshStartup() string {
	dir := s.Home
	if z := s.Getenv("ZDOTDIR"); z != "" && filepath.IsAbs(z) {
		dir = z
	}
	return filepath.Join(dir, ".zshrc")
}

// shQuote quotes p for a double-quoted sh word, writing the home
// directory as $HOME.
func (s *System) shQuote(p string) string {
	home := ""
	if rest, ok := strings.CutPrefix(p, s.Home+"/"); ok {
		home, p = "$HOME/", rest
	}
	return `"` + home + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(p) + `"`
}

// fishQuote quotes p for a double-quoted fish word, writing the home
// directory as $HOME.
func (s *System) fishQuote(p string) string {
	home := ""
	if rest, ok := strings.CutPrefix(p, s.Home+"/"); ok {
		home, p = "$HOME/", rest
	}
	return `"` + home + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(p) + `"`
}

// binComment explains the block that puts the install directory on PATH.
const binComment = "talaria's install directory, for the talaria and tal commands in a terminal and in agent commands."

// envBlock adds dirs to PATH, in this order, when they exist and are not
// on PATH yet.
func (s *System) envBlock(shell string, dirs []string) string {
	return s.pathBlock(fmt.Sprintf("PATH directories that %s has and bash -l lacked. talaria runs agent commands with bash -l.", shell), dirs)
}

func (s *System) pathBlock(comment string, dirs []string) string {
	var b strings.Builder
	b.WriteString(blockBegin + "\n")
	b.WriteString("# " + comment + "\n")
	b.WriteString("for " + pathVar + " in")
	for _, d := range slices.Backward(dirs) {
		b.WriteString(" " + s.shQuote(d))
	}
	b.WriteString("; do\n" +
		"    case \":$PATH:\" in\n" +
		"        *\":$" + pathVar + ":\"*) ;;\n" +
		"        *) [ -d \"$" + pathVar + "\" ] && PATH=\"$" + pathVar + ":$PATH\" ;;\n" +
		"    esac\n" +
		"done\n" +
		"unset " + pathVar + "\n" +
		"export PATH\n" +
		blockEnd + "\n")
	return b.String()
}

// sourceBlock sources env. In a ~/.bash_profile that setup creates, it
// also sources ~/.profile, which bash would read without that file.
func (s *System) sourceBlock(env string, profile bool) string {
	q := s.shQuote(env)
	b := blockBegin + "\n"
	if profile {
		p := s.shQuote(filepath.Join(s.Home, ".profile"))
		b += "[ -f " + p + " ] && . " + p + "\n"
	}
	return b + "[ -f " + q + " ] && . " + q + "\n" + blockEnd + "\n"
}

// fishBlock puts dir first on fish's PATH when it exists and is not on it.
func (s *System) fishBlock(dir string) string {
	q := s.fishQuote(dir)
	return blockBegin + "\n# " + binComment + "\n" +
		"if test -d " + q + "; and not contains -- " + q + " $PATH\n" +
		"    set -gx PATH " + q + " $PATH\n" +
		"end\n" + blockEnd + "\n"
}

// ShellPlan is the change FixShell or FixBinDir makes.
type ShellPlan struct {
	// EnvFile gets the PATH block. Empty when only Fish changes.
	EnvFile string
	// Sources are the startup files that get a line that sources EnvFile.
	Sources []string
	// Fish is fish's file that gets the install directory, when fish
	// lacks it.
	Fish string
}

// Files lists every file the plan changes.
func (p ShellPlan) Files() []string {
	var f []string
	if p.EnvFile != "" {
		f = append(f, p.EnvFile)
	}
	f = append(f, p.Sources...)
	if p.Fish != "" {
		f = append(f, p.Fish)
	}
	return f
}

// PlanShell returns the files FixShell changes for r.
func (s *System) PlanShell(r *ShellReport) ShellPlan {
	files := []string{s.BashStartup()}
	if r.Shell == shellZsh {
		files = append(files, s.ZshStartup())
	}
	return s.planSources(files)
}

// PlanBinDir returns the files FixBinDir changes for r: the shared
// environment file and the startup files of each shell that lacks the
// install directory, or fish's own file. It is empty when no shell lacks
// it.
func (s *System) PlanBinDir(r *ShellReport) ShellPlan {
	var files []string
	if r.BashLacksBin {
		files = append(files, s.BashStartup())
	}
	var fish string
	if r.LoginLacksBin {
		switch r.Login {
		case shellZsh:
			files = append(files, s.ZshStartup())
		case shellBash:
			files = append(files, s.BashStartup(), s.BashRC())
		case shellFish:
			fish = s.FishConf()
		}
	}
	var p ShellPlan
	if len(files) > 0 {
		p = s.planSources(slices.Compact(files))
	}
	p.Fish = fish
	return p
}

func (s *System) planSources(files []string) ShellPlan {
	p := ShellPlan{EnvFile: s.EnvFile()}
	ref := strings.TrimPrefix(p.EnvFile, s.Home+"/")
	for _, f := range files {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), ref) {
			continue
		}
		p.Sources = append(p.Sources, f)
	}
	return p
}

// FixShell appends the PATH block to the env file and sources it from the
// startup files of plan. Each existing file is backed up first, and every
// change is recorded in st.
func (s *System) FixShell(r *ShellReport, plan ShellPlan, st *State) error {
	return s.fixPlan(plan, s.envBlock(r.Shell, r.Missing), st)
}

// FixBinDir puts r.BinDir on PATH in the files of plan, as FixShell does.
func (s *System) FixBinDir(r *ShellReport, plan ShellPlan, st *State) error {
	if plan.Fish != "" {
		if err := s.appendBlock(plan.Fish, s.fishBlock(r.BinDir), st); err != nil {
			return err
		}
	}
	if plan.EnvFile == "" {
		return nil
	}
	return s.fixPlan(plan, s.pathBlock(binComment, []string{r.BinDir}), st)
}

func (s *System) fixPlan(plan ShellPlan, block string, st *State) error {
	if err := s.appendBlock(plan.EnvFile, block, st); err != nil {
		return err
	}
	for _, f := range plan.Sources {
		_, err := os.Lstat(f)
		profile := errors.Is(err, fs.ErrNotExist) && filepath.Base(f) == ".bash_profile"
		if err := s.appendBlock(f, s.sourceBlock(plan.EnvFile, profile), st); err != nil {
			return err
		}
	}
	return nil
}

func (s *System) appendBlock(path, block string, st *State) error {
	old, err := os.ReadFile(path)
	created := errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return err
	}
	if fi, lerr := os.Lstat(path); created && lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link to a missing file; left it alone", path)
	}
	if strings.Contains(string(old), block) || strings.Contains(string(old), legacyBlock(block)) {
		return nil
	}
	e := Edit{File: path, Created: created}
	mode := os.FileMode(0o644)
	if !created {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
		if e.Backup, err = s.backup(path); err != nil {
			return err
		}
	}
	data := string(old)
	if data != "" && !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	if data != "" {
		data += "\n"
	}
	data += block
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(data), mode); err != nil {
		return err
	}
	if prev := st.edit(path); prev != nil {
		prev.Created = prev.Created || created
		return nil
	}
	st.Edits = append(st.Edits, e)
	return nil
}

// RemoveBlocks takes every setup block out of path. It deletes a file
// that setup created and that is empty afterwards.
func RemoveBlocks(path string, created bool) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var out []string
	in, changed, end := false, false, ""
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case !in && (line == blockBegin || line == legacyBlockBegin):
			in, changed, end = true, true, blockEnd
			if line == legacyBlockBegin {
				end = legacyBlockEnd
			}
			if n := len(out); n > 0 && out[n-1] == "" {
				out = out[:n-1]
			}
		case in && line == end:
			in = false
		case !in:
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if in {
		return false, fmt.Errorf("%s: a talaria setup block has no end marker; left it alone", path)
	}
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	if created && strings.TrimSpace(text) == "" {
		return true, os.Remove(path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return true, writeFileAtomic(path, []byte(text), fi.Mode().Perm())
}
