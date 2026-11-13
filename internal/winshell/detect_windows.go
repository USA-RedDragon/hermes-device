//go:build windows

package winshell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// Found is what is installed.
type Found struct {
	// Bash is Git Bash's bash.exe, empty when Git for Windows is missing.
	Bash string
	// PowerShell is pwsh.exe when PowerShell 7 is installed, else
	// Windows PowerShell.
	PowerShell string
	// WSL is wsl.exe and Distros its installed distributions, the default
	// one first; empty when WSL has none.
	WSL     string
	Distros []string
}

// Kinds returns the shells that can run, in the order setup offers them.
// PowerShell needs Git Bash too: the plugin's file tools run bash scripts.
func (f Found) Kinds() []string {
	var out []string
	if f.Bash != "" {
		out = append(out, GitBash)
		if f.PowerShell != "" {
			out = append(out, PowerShell)
		}
	}
	if f.WSL != "" && len(f.Distros) > 0 {
		out = append(out, WSL)
	}
	return out
}

// Default is the shell setup picks without a choice: Git Bash when it is
// there, else PowerShell, else WSL.
func (f Found) Default() string {
	if k := f.Kinds(); len(k) > 0 {
		return k[0]
	}
	return ""
}

// Detect looks for Git Bash, PowerShell and WSL distributions.
func Detect(ctx context.Context) Found {
	f := Found{Bash: findGitBash(), PowerShell: findPowerShell()}
	if wsl, err := exec.LookPath("wsl.exe"); err == nil {
		f.WSL = wsl
		f.Distros = wslDistros(ctx, wsl)
	}
	return f
}

// Resolve returns the shell kind with distro (WSL only; empty is the
// default distro). It fails when what the kind needs is missing.
func Resolve(ctx context.Context, kind, distro string) (Shell, error) {
	f := Detect(ctx)
	s := Shell{Kind: kind}
	switch kind {
	case GitBash, PowerShell:
		if f.Bash == "" {
			return s, errors.New("Git Bash is not installed: install Git for Windows (winget install Git.Git); " +
				"the " + kind + " shell needs its bash for the file tools")
		}
		s.Bash = f.Bash
		if kind == PowerShell {
			s.PowerShell = f.PowerShell
		}
	case WSL:
		if f.WSL == "" || len(f.Distros) == 0 {
			return s, errors.New("WSL has no Linux distribution: run wsl --install -d Debian")
		}
		s.WSL = f.WSL
		if distro != "" && !contains(f.Distros, distro) {
			return s, fmt.Errorf("WSL has no distribution %q (found %s)", distro, strings.Join(f.Distros, ", "))
		}
		s.Distro = distro
		if root := wslMountRoot(ctx, s); root != "" {
			s.MountRoot = root
		}
	default:
		return s, fmt.Errorf("unknown shell %q: use one of %s", kind, strings.Join(Kinds(), ", "))
	}
	return s, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func findGitBash() string {
	var dirs []string
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		if k, err := registry.OpenKey(root, `SOFTWARE\GitForWindows`, registry.QUERY_VALUE); err == nil {
			if p, _, err := k.GetStringValue("InstallPath"); err == nil {
				dirs = append(dirs, p)
			}
			_ = k.Close()
		}
	}
	if git, err := exec.LookPath("git.exe"); err == nil {
		// git.exe is in <root>\cmd or <root>\bin.
		dirs = append(dirs, filepath.Dir(filepath.Dir(git)))
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "LOCALAPPDATA"} {
		if v := os.Getenv(env); v != "" {
			sub := "Git"
			if env == "LOCALAPPDATA" {
				sub = filepath.Join("Programs", "Git")
			}
			dirs = append(dirs, filepath.Join(v, sub))
		}
	}
	for _, d := range dirs {
		if st, err := fs.Stat(os.DirFS(d), "bin/bash.exe"); err == nil && !st.IsDir() {
			return filepath.Join(d, "bin", "bash.exe")
		}
	}
	return ""
}

func findPowerShell() string {
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		if _, err := fs.Stat(os.DirFS(root), "System32/WindowsPowerShell/v1.0/powershell.exe"); err == nil {
			return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		}
	}
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	return ""
}

// wslDistros lists the distributions, default first. wsl.exe writes its
// own output in UTF-16.
func wslDistros(ctx context.Context, wsl string) []string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := runWSL(ctx, wsl, "--list", "--quiet")
	if err != nil {
		return nil
	}
	var list []string
	for line := range strings.SplitSeq(decodeWSL(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			list = append(list, line)
		}
	}
	return list
}

// runWSL runs wsl.exe with args and returns its output.
func runWSL(ctx context.Context, wsl string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, wsl, args...).Output()
}

// decodeWSL decodes wsl.exe's UTF-16LE output; output that is not UTF-16
// (WSL_UTF8=1) is returned as it is.
func decodeWSL(b []byte) string {
	if len(b) < 2 || len(b)%2 != 0 || b[1] != 0 {
		return strings.ReplaceAll(string(b), "\r", "")
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return strings.ReplaceAll(strings.TrimPrefix(string(utf16.Decode(u)), "\ufeff"), "\r", "")
}

// wslMountRoot asks the distro where C: is, for a distro whose
// /etc/wsl.conf moves the drive mounts.
func wslMountRoot(ctx context.Context, s Shell) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := []string{}
	if s.Distro != "" {
		argv = append(argv, "-d", s.Distro)
	}
	argv = append(argv, "--exec", "wslpath", "-u", `C:\`)
	out, err := runWSL(ctx, s.WSL, argv...)
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	root, ok := strings.CutSuffix(strings.TrimSuffix(p, "/"), "/c")
	if !ok || !strings.HasPrefix(root, "/") {
		return ""
	}
	return root + "/"
}
