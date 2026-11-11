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
	blockBegin = "# >>> hermes-device setup >>>"
	blockEnd   = "# <<< hermes-device setup <<<"
	pathMarker = "__HERMES_DEVICE_PATH__="
)

// basePath is the PATH that the service manager gives the daemon, before
// bash -l adds to it.
const basePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

const shellTimeout = 15 * time.Second

// ShellReport compares the login shell's PATH with what agent commands get.
type ShellReport struct {
	// Shell is the login shell's base name: zsh or fish. Empty when the
	// login shell is bash or another shell that setup does not check.
	Shell string
	// Missing are directories on the login shell's PATH that bash -l does
	// not have, in the login shell's order.
	Missing []string
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

// CheckShell runs the login shell (interactive, as in a terminal) and bash
// -l (as agent commands run) and lists the directories bash lacks.
func (s *System) CheckShell(ctx context.Context) (*ShellReport, error) {
	r := &ShellReport{}
	name := filepath.Base(s.Shell)
	var args []string
	switch name {
	case "zsh":
		args = []string{"-l", "-i", "-c", `printf '\n` + pathMarker + `%s\n' "$PATH"`}
	case "fish":
		args = []string{"-l", "-i", "-c", `printf '\n` + pathMarker + `%s\n' (string join : $PATH)`}
	default:
		return r, nil
	}
	r.Shell = name
	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	out, err := s.Run.Output(ctx, s.shellEnv(), s.Shell, args...)
	login := parsePath(out)
	if login == nil {
		return nil, fmt.Errorf("read PATH from %s: %w", s.Shell, err)
	}
	out, err = s.Run.Output(ctx, s.shellEnv(), "bash", "-l", "-c", `printf '\n`+pathMarker+`%s\n' "$PATH"`)
	bash := parsePath(out)
	if bash == nil {
		return nil, fmt.Errorf("read PATH from bash -l: %w", err)
	}
	for _, d := range login {
		if d == "" || !filepath.IsAbs(d) || slices.Contains(bash, d) || slices.Contains(r.Missing, d) {
			continue
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			r.Missing = append(r.Missing, d)
		}
	}
	return r, nil
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

// envBlock adds dirs to PATH, in this order, when they exist and are not
// on PATH yet.
func (s *System) envBlock(shell string, dirs []string) string {
	var b strings.Builder
	b.WriteString(blockBegin + "\n")
	fmt.Fprintf(&b, "# PATH directories that %s has and bash -l lacked. hermes-device runs agent commands with bash -l.\n", shell)
	b.WriteString("for hermes_device_dir in")
	for _, d := range slices.Backward(dirs) {
		b.WriteString(" " + s.shQuote(d))
	}
	b.WriteString("; do\n" +
		"    case \":$PATH:\" in\n" +
		"        *\":$hermes_device_dir:\"*) ;;\n" +
		"        *) [ -d \"$hermes_device_dir\" ] && PATH=\"$hermes_device_dir:$PATH\" ;;\n" +
		"    esac\n" +
		"done\n" +
		"unset hermes_device_dir\n" +
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

// ShellPlan is the change FixShell makes.
type ShellPlan struct {
	EnvFile string
	// Sources are the startup files that get a line that sources EnvFile.
	Sources []string
}

// PlanShell returns the files FixShell changes for r.
func (s *System) PlanShell(r *ShellReport) ShellPlan {
	p := ShellPlan{EnvFile: s.EnvFile()}
	files := []string{s.BashStartup()}
	if r.Shell == "zsh" {
		files = append(files, s.ZshStartup())
	}
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
	if err := s.appendBlock(plan.EnvFile, s.envBlock(r.Shell, r.Missing), st); err != nil {
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
	if strings.Contains(string(old), block) {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a shell file
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
	in, changed := false, false
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case !in && line == blockBegin:
			in, changed = true, true
			if n := len(out); n > 0 && out[n-1] == "" {
				out = out[:n-1]
			}
		case in && line == blockEnd:
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
		return false, fmt.Errorf("%s: a hermes-device setup block has no end marker; left it alone", path)
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
