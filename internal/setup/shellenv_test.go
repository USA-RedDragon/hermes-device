package setup_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

const (
	envSh  = ".config/shell/env.sh"
	usrBin = "/usr/bin"
)

// pathRunner answers each shell with the PATH in paths.
func pathRunner(paths map[string]string) *fakeRunner {
	return &fakeRunner{respond: func(name string, _ []string) ([]byte, error) {
		if p, ok := paths[name]; ok {
			return []byte("\n__HERMES_DEVICE_PATH__=" + p + "\n"), nil
		}
		return nil, errNotFound
	}}
}

func TestCheckShellBinDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		shell, login, bash string
		loginLacks, bash0  bool
		files              []string
	}{
		{shell: zsh, login: usrBin, bash: usrBin, loginLacks: true, bash0: true, files: []string{envSh, bashProfile, ".zshrc"}},
		{shell: zsh, login: "BIN/:/usr/bin", bash: "/usr/bin:BIN", files: nil},
		{shell: zsh, login: "BIN:/usr/bin", bash: usrBin, bash0: true, files: []string{envSh, bashProfile}},
		{shell: bash, login: usrBin, bash: usrBin, loginLacks: true, bash0: true, files: []string{envSh, bashProfile, ".bashrc"}},
		{shell: fish, login: usrBin, bash: "BIN:/usr/bin", loginLacks: true, files: []string{".config/fish/conf.d/talaria.fish"}},
		{shell: "sh", bash: usrBin, bash0: true, files: []string{envSh, bashProfile}},
	} {
		sys := newSystem(t, linux, &fakeRunner{}, nil, nil)
		bin := filepath.Join(sys.Home, ".talaria", "bin")
		sys.Run = pathRunner(map[string]string{
			tc.shell: strings.ReplaceAll(tc.login, "BIN", bin),
			bash:     strings.ReplaceAll(tc.bash, "BIN", bin),
		})
		sys.Shell = "/usr/bin/" + tc.shell
		rep, err := sys.CheckShell(t.Context(), bin)
		if err != nil {
			t.Fatalf("%s: %v", tc.shell, err)
		}
		if rep.Login != tc.shell || rep.LoginLacksBin != tc.loginLacks || rep.BashLacksBin != tc.bash0 || len(rep.Missing) != 0 {
			t.Errorf("%s %q: report %+v", tc.shell, tc.login, rep)
		}
		files := sys.PlanBinDir(rep).Files()
		got := make([]string, 0, len(files))
		for _, f := range files {
			got = append(got, strings.TrimPrefix(f, sys.Home+"/"))
		}
		if strings.Join(got, " ") != strings.Join(tc.files, " ") {
			t.Errorf("%s %q: plan %q, want %q", tc.shell, tc.login, got, tc.files)
		}
	}
}

// TestSetupAddsBinDirToSourcedEnvFile is a zsh user whose .zshrc and
// .bash_profile already source env.sh: only env.sh changes, once.
func TestSetupAddsBinDirToSourcedEnvFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.opts.NoBrowser, f.opts.NoService = true, true
	home := f.sys.Home
	source := "[ -f \"$HOME/.config/shell/env.sh\" ] && . \"$HOME/.config/shell/env.sh\"\n"
	env := "export PATH=\"$HOME/.local/bin\":$PATH\n"
	writeFile(t, filepath.Join(home, ".zshrc"), zshrcText+source)
	writeFile(t, filepath.Join(home, bashProfile), source)
	writeFile(t, filepath.Join(home, ".config", "shell", "env.sh"), env)
	f.sys.Interactive, f.sys.In = true, strings.NewReader("y\n")
	rep, err := f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(home, ".zshrc")) != zshrcText+source || readFile(t, filepath.Join(home, bashProfile)) != source {
		t.Error("changed a startup file that already sources env.sh")
	}
	got := readFile(t, filepath.Join(home, ".config", "shell", "env.sh"))
	if !strings.HasPrefix(got, env+"\n# >>> hermes-device setup >>>\n# talaria's install directory") ||
		!strings.Contains(got, `for hermes_device_dir in "$HOME/.talaria/bin"; do`) {
		t.Errorf("env.sh:\n%s", got)
	}
	out, _ := f.sys.Out.(*bytes.Buffer)
	if !strings.Contains(out.String(), "not on PATH for zsh and bash -l") || !strings.Contains(out.String(), "(it is already sourced by your startup files)") {
		t.Errorf("output %q", out)
	}
	if !strings.Contains(strings.Join(rep.Done, "\n"), "Added "+filepath.Join(home, ".talaria", "bin")+" to PATH in "+filepath.Join(home, ".config", "shell", "env.sh")) {
		t.Errorf("report %+v", rep)
	}
	out.Reset()
	f.sys.In = strings.NewReader("")
	rep, err = f.sys.Setup(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[y/N]") || readFile(t, filepath.Join(home, ".config", "shell", "env.sh")) != got {
		t.Errorf("second run asked or changed env.sh: %q", out)
	}
	if !strings.Contains(strings.Join(rep.Done, "\n"), filepath.Join(home, ".talaria", "bin")+" is on PATH") {
		t.Errorf("second report %+v", rep)
	}
}

func TestFixBinDirFish(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, nil)
	bin := filepath.Join(sys.Home, ".talaria", "bin")
	rep := &setup.ShellReport{Login: fish, BinDir: bin, LoginLacksBin: true}
	plan := sys.PlanBinDir(rep)
	st := &setup.State{}
	for range 2 {
		if err := sys.FixBinDir(rep, plan, st); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(sys.Home, ".config", "fish", "conf.d", "talaria.fish")
	want := "# >>> hermes-device setup >>>\n# talaria's install directory, for the talaria and tal commands in a terminal and in agent commands.\n" +
		"if test -d \"$HOME/.talaria/bin\"; and not contains -- \"$HOME/.talaria/bin\" $PATH\n" +
		"    set -gx PATH \"$HOME/.talaria/bin\" $PATH\nend\n# <<< hermes-device setup <<<\n"
	if got := readFile(t, conf); got != want {
		t.Errorf("talaria.fish:\n%s", got)
	}
	if exists(sys.EnvFile()) || len(st.Edits) != 1 || !st.Edits[0].Created {
		t.Errorf("edits %+v", st.Edits)
	}
	if changed, err := setup.RemoveBlocks(conf, true); err != nil || !changed || exists(conf) {
		t.Errorf("RemoveBlocks = %t, %v", changed, err)
	}
}

func TestOnPath(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, map[string]string{"PATH": "/usr/bin:/home/alice/.talaria/bin/"})
	if !sys.OnPath("/home/alice/.talaria/bin") || sys.OnPath("/home/alice/.local/bin") {
		t.Error("OnPath")
	}
}
