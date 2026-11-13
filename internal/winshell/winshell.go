// Package winshell is how talaria runs commands on Windows: the shell the
// user chose, the bash that runs the plugin's own scripts, and the paths
// the device shows to the server.
//
// The plugin and Hermes speak POSIX paths and wrap every command in a bash
// script, so a Windows device always runs the wire command with a bash:
// Git Bash for the git-bash and powershell shells, the WSL distro's bash
// for wsl. Paths on the wire are the ones that bash understands: /c/Users/x
// for Git Bash, /mnt/c/Users/x for WSL. With the powershell shell the
// plugin wraps the agent's own commands in a PowerShell call.
package winshell

import (
	"errors"
	"fmt"
	"strings"
)

// Shell kinds, the values of the device.shell setting on Windows.
const (
	GitBash    = "git-bash"
	PowerShell = "powershell"
	WSL        = "wsl"
)

// Kinds lists the shell kinds in the order setup offers them.
func Kinds() []string { return []string{GitBash, PowerShell, WSL} }

// Valid reports whether kind is one of Kinds.
func Valid(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// ErrNotWindowsPath is returned for a wire path that names no Windows
// drive folder, such as /etc in WSL.
var ErrNotWindowsPath = errors.New("not a path on a Windows drive")

// Shell is a resolved shell.
type Shell struct {
	// Kind is GitBash, PowerShell or WSL.
	Kind string
	// Bash is Git's bash.exe, for GitBash and PowerShell.
	Bash string
	// PowerShell is pwsh.exe or powershell.exe, for PowerShell.
	PowerShell string
	// WSL is wsl.exe and Distro the distribution, for WSL. An empty
	// Distro is the default one.
	WSL, Distro string
	// MountRoot is where WSL mounts the drives, /mnt/ by default.
	MountRoot string
}

// Argv is the command line that runs the wire command cmd (a bash script)
// in the folder cwd, a Windows path.
func (s Shell) Argv(cmd string, login bool, cwd string) []string {
	if s.Kind == WSL {
		argv := []string{s.WSL}
		if s.Distro != "" {
			argv = append(argv, "-d", s.Distro)
		}
		argv = append(argv, "--cd", cwd, "--exec", "bash")
		if login {
			argv = append(argv, "-l")
		}
		return append(argv, "-c", cmd)
	}
	argv := []string{s.Bash}
	if login {
		argv = append(argv, "-l")
	}
	return append(argv, "-c", cmd)
}

// Env is what the shell adds to each command's environment.
func (s Shell) Env() []string {
	switch s.Kind {
	case GitBash, PowerShell:
		// CHERE_INVOKING keeps Git Bash's login profile in the command's
		// folder instead of going to the home folder.
		env := []string{"CHERE_INVOKING=1"}
		if s.PowerShell != "" {
			env = append(env, "TALARIA_POWERSHELL="+s.ToWire(s.PowerShell))
		}
		return env
	}
	return nil
}

func (s Shell) mountRoot() string {
	if s.MountRoot == "" {
		return "/mnt/"
	}
	return strings.TrimSuffix(s.MountRoot, "/") + "/"
}

// ToWire turns the Windows path p (C:\Users\x) into the path the shell's
// bash uses (/c/Users/x, or /mnt/c/Users/x in WSL). A path that is not on
// a drive is returned with forward slashes.
func (s Shell) ToWire(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 2 && p[1] == ':' && isLetter(p[0]) {
		rest := strings.TrimSuffix(p[2:], "/")
		drive := strings.ToLower(p[:1])
		if s.Kind == WSL {
			return s.mountRoot() + drive + rest
		}
		return "/" + drive + rest
	}
	return p
}

// FromWire turns a path of the shell's bash back into a Windows path.
func (s Shell) FromWire(p string) (string, error) {
	rest, ok := p, true
	if s.Kind == WSL {
		rest, ok = strings.CutPrefix(p, s.mountRoot())
		rest = "/" + rest
	}
	if !ok || len(rest) < 2 || rest[0] != '/' || !isLetter(rest[1]) || (len(rest) > 2 && rest[2] != '/') {
		return "", fmt.Errorf("%q: %w", p, ErrNotWindowsPath)
	}
	out := strings.ToUpper(rest[1:2]) + `:\` + strings.ReplaceAll(strings.TrimPrefix(rest[2:], "/"), "/", `\`)
	return out, nil
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// Describe is how the shell is named to the agent and the user.
func (s Shell) Describe() string {
	switch s.Kind {
	case GitBash:
		return "Git Bash"
	case PowerShell:
		if strings.Contains(strings.ToLower(s.PowerShell), "pwsh") {
			return "PowerShell 7"
		}
		return "Windows PowerShell"
	case WSL:
		if s.Distro != "" {
			return "WSL (" + s.Distro + ")"
		}
		return "WSL"
	}
	return s.Kind
}
