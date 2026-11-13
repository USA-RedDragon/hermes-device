package winshell_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/winshell"
)

func TestPaths(t *testing.T) {
	t.Parallel()
	git := winshell.Shell{Kind: winshell.GitBash}
	wsl := winshell.Shell{Kind: winshell.WSL}
	moved := winshell.Shell{Kind: winshell.WSL, MountRoot: "/"}
	for _, tc := range []struct {
		sh          winshell.Shell
		local, wire string
	}{
		{git, `C:\Users\alice\Projects`, "/c/Users/alice/Projects"},
		{git, `C:\`, "/c"},
		{git, `d:\work`, "/d/work"},
		{wsl, `C:\Users\alice`, "/mnt/c/Users/alice"},
		{wsl, `E:\`, "/mnt/e"},
		{moved, `C:\Users\alice`, "/c/Users/alice"},
	} {
		if got := tc.sh.ToWire(tc.local); got != tc.wire {
			t.Errorf("%s ToWire(%q) = %q, want %q", tc.sh.Kind, tc.local, got, tc.wire)
		}
		back, err := tc.sh.FromWire(tc.wire)
		want := tc.local
		if want[0] >= 'a' {
			want = string(want[0]-'a'+'A') + want[1:]
		}
		if err != nil || back != want {
			t.Errorf("%s FromWire(%q) = %q, %v, want %q", tc.sh.Kind, tc.wire, back, err, want)
		}
	}
	for _, tc := range []struct {
		sh   winshell.Shell
		wire string
	}{
		{git, "/usr/bin"},
		{git, "relative/path"},
		{git, "/"},
		{wsl, "/home/alice"},
		{wsl, "/c/Users"},
		{wsl, "/mnt/"},
	} {
		if got, err := tc.sh.FromWire(tc.wire); !errors.Is(err, winshell.ErrNotWindowsPath) {
			t.Errorf("%s FromWire(%q) = %q, %v, want ErrNotWindowsPath", tc.sh.Kind, tc.wire, got, err)
		}
	}
}

func TestArgv(t *testing.T) {
	t.Parallel()
	git := winshell.Shell{Kind: winshell.GitBash, Bash: `C:\Program Files\Git\bin\bash.exe`}
	if got, want := git.Argv("pwd", true, `C:\x`), []string{git.Bash, "-l", "-c", "pwd"}; !slices.Equal(got, want) {
		t.Errorf("git-bash argv = %q, want %q", got, want)
	}
	ps := winshell.Shell{Kind: winshell.PowerShell, Bash: git.Bash, PowerShell: `C:\Program Files\PowerShell\7\pwsh.exe`}
	if got, want := ps.Argv("pwd", false, `C:\x`), []string{git.Bash, "-c", "pwd"}; !slices.Equal(got, want) {
		t.Errorf("powershell argv = %q, want %q", got, want)
	}
	if !slices.Contains(ps.Env(), "TALARIA_POWERSHELL=/c/Program Files/PowerShell/7/pwsh.exe") {
		t.Errorf("powershell env = %q", ps.Env())
	}
	wsl := winshell.Shell{Kind: winshell.WSL, WSL: `C:\Windows\System32\wsl.exe`, Distro: "Ubuntu"}
	want := []string{wsl.WSL, "-d", "Ubuntu", "--cd", `C:\x`, "--exec", "bash", "-l", "-c", "pwd"}
	if got := wsl.Argv("pwd", true, `C:\x`); !slices.Equal(got, want) {
		t.Errorf("wsl argv = %q, want %q", got, want)
	}
	if wsl.Env() != nil {
		t.Errorf("wsl env = %q, want none", wsl.Env())
	}
}

func TestValid(t *testing.T) {
	t.Parallel()
	for _, k := range winshell.Kinds {
		if !winshell.Valid(k) {
			t.Errorf("Valid(%q) = false", k)
		}
	}
	if winshell.Valid("bash") || winshell.Valid("cmd") {
		t.Error("Valid accepts an unknown kind")
	}
}
