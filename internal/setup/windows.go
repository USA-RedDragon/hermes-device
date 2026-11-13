package setup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Windows shell kinds, as internal/winshell names them.
const (
	shellGitBash    = "git-bash"
	shellPowerShell = "powershell"
	shellWSL        = "wsl"
)

// shellMeaning tells the user what each Windows shell means for the agent.
func shellMeaning(kind string) string {
	switch kind {
	case shellGitBash:
		return "Git Bash: the agent writes bash commands; Windows programs and files are at /c/..."
	case shellPowerShell:
		return "PowerShell: the agent writes PowerShell commands (file tools still use Git Bash)"
	case shellWSL:
		return "WSL: the agent writes bash commands that run in Linux; your Windows files are at /mnt/c/..."
	}
	return kind
}

// ErrNoWindowsShell is returned when Windows has no shell talaria can use.
var ErrNoWindowsShell = errors.New("no shell for agent commands: install Git for Windows (winget install Git.Git) " +
	"or a WSL distribution (wsl --install -d Ubuntu), then run setup again")

// chooseShell picks the Windows shell for a new config: o.Shell when
// given, else the user's answer, else the first one of o.Shells.
func (s *System) chooseShell(ctx context.Context, o *Options) (string, error) {
	if len(o.Shells) == 0 {
		return "", ErrNoWindowsShell
	}
	if o.Shell != "" {
		for _, k := range o.Shells {
			if k == o.Shell {
				return k, nil
			}
		}
		return "", fmt.Errorf("--shell %s is not available here; this computer has %s", o.Shell, strings.Join(o.Shells, ", "))
	}
	if len(o.Shells) == 1 || !s.Interactive {
		return o.Shells[0], nil
	}
	s.printf("\nWhich shell should the agent's commands run in?\n")
	for i, k := range o.Shells {
		def := ""
		if i == 0 {
			def = " (default)"
		}
		s.printf("  %d. %s%s\n", i+1, shellMeaning(k), def)
	}
	for {
		s.printf("Shell [1-%d, Enter for 1]: ", len(o.Shells))
		line, err := s.readLine(ctx)
		if err != nil {
			return "", err
		}
		a := strings.TrimSpace(line)
		if a == "" {
			return o.Shells[0], nil
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(o.Shells) {
			return o.Shells[n-1], nil
		}
		for _, k := range o.Shells {
			if strings.EqualFold(a, k) {
				return k, nil
			}
		}
	}
}

// userPath reads the user's own PATH (HKCU\Environment), without the
// system part.
func (s *System) userPath(ctx context.Context) (string, error) {
	out, err := s.Run.Output(ctx, nil, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"[Environment]::GetEnvironmentVariable('Path','User')")
	if err != nil {
		return "", fmt.Errorf("read the user PATH: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// setUserPath writes the user's PATH; Windows tells running programs, and
// new terminals see it.
func (s *System) setUserPath(ctx context.Context, value string) error {
	cmd := "[Environment]::SetEnvironmentVariable('Path'," + psQuote(value) + ",'User')"
	if _, err := s.Run.Output(ctx, nil, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd); err != nil {
		return fmt.Errorf("write the user PATH: %w", err)
	}
	return nil
}

// psQuote quotes s for PowerShell as a literal string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func pathHas(list, dir string) bool {
	for _, d := range strings.Split(list, ";") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(d), `\`), strings.TrimRight(dir, `\`)) {
			return true
		}
	}
	return false
}

// windowsPathStep puts the install directory on the user's PATH.
func (s *System) windowsPathStep(ctx context.Context, st *State, r *Report, dir string) error {
	cur, err := s.userPath(ctx)
	if err != nil {
		r.todo("Add %s to your PATH (setup could not read it: %v)", dir, err)
		return nil
	}
	if pathHas(cur, dir) {
		r.done("%s is on your PATH", dir)
		return nil
	}
	next := dir
	if cur != "" {
		next = strings.TrimRight(cur, ";") + ";" + dir
	}
	if err := s.setUserPath(ctx, next); err != nil {
		r.todo("Add %s to your PATH (%v)", dir, err)
		return nil
	}
	st.UserPath = dir
	r.done("Added %s to your PATH; terminals you open from now on find talaria", dir)
	return nil
}

// uninstallUserPath takes the directory setup added out of the user PATH.
func (s *System) uninstallUserPath(ctx context.Context, st *State, r *Report) error {
	if st.UserPath == "" {
		return nil
	}
	cur, err := s.userPath(ctx)
	if err != nil {
		return err
	}
	var keep []string
	for _, d := range strings.Split(cur, ";") {
		if d != "" && !strings.EqualFold(strings.TrimRight(d, `\`), strings.TrimRight(st.UserPath, `\`)) {
			keep = append(keep, d)
		}
	}
	if err := s.setUserPath(ctx, strings.Join(keep, ";")); err != nil {
		return err
	}
	r.done("Took %s out of your PATH", st.UserPath)
	st.UserPath = ""
	return nil
}
