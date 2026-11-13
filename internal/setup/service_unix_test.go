//go:build unix

package setup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

func TestCheckShellFish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &fakeRunner{respond: func(name string, _ []string) ([]byte, error) {
		if name == fish {
			return []byte("__TALARIA_PATH__=" + dir + ":/usr/bin\n"), nil
		}
		return []byte("__TALARIA_PATH__=/usr/bin\n"), nil
	}}
	sys := newSystem(t, linux, r, nil, nil)
	sys.Shell = "/usr/bin/fish"
	rep, err := sys.CheckShell(t.Context(), "")
	if err != nil || rep.Shell != fish || len(rep.Missing) != 1 || rep.Missing[0] != dir {
		t.Errorf("CheckShell = %+v, %v", rep, err)
	}
	sys.Shell = "/bin/bash"
	if rep, err := sys.CheckShell(t.Context(), ""); err != nil || rep.Shell != "" {
		t.Errorf("bash login shell: %+v, %v", rep, err)
	}
	r.respond = func(string, []string) ([]byte, error) { return []byte("no marker"), errNotFound }
	sys.Shell = "/usr/bin/zsh"
	if _, err := sys.CheckShell(t.Context(), ""); err == nil {
		t.Error("no error without a PATH line")
	}
}

func TestShellFixKeepsSymlinkedDotfiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	home := f.sys.Home
	target := filepath.Join(home, "dotfiles", "zshrc")
	writeFile(t, target, zshrcText)
	if err := os.Remove(filepath.Join(home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".profile"), "umask 022\n")
	rep := &setup.ShellReport{Shell: zsh, Missing: []string{filepath.Join(home, ".local", "bin"), "/opt/a b/bin"}}
	plan := f.sys.PlanShell(rep)
	if len(plan.Sources) != 2 || plan.Sources[0] != filepath.Join(home, ".profile") {
		t.Fatalf("plan %+v", plan)
	}
	st := &setup.State{}
	if err := f.sys.FixShell(rep, plan, st); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(home, ".zshrc")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error(".zshrc is no longer a symbolic link")
	}
	if !strings.Contains(readFile(t, target), "env.sh") {
		t.Error("the link's target lacks the source line")
	}
	env := readFile(t, filepath.Join(home, ".config", "shell", "env.sh"))
	if !strings.Contains(env, `for talaria_dir in "/opt/a b/bin" "$HOME/.local/bin"; do`) {
		t.Errorf("env.sh:\n%s", env)
	}
	if len(st.Edits) != 3 || !st.Edits[0].Created || st.Edits[1].Backup == "" {
		t.Errorf("edits %+v", st.Edits)
	}
	if again := f.sys.PlanShell(rep); len(again.Sources) != 0 {
		t.Errorf("second plan %+v", again)
	}
}

func TestSystemdUnitPaths(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, &fakeRunner{}, nil, map[string]string{"XDG_CONFIG_HOME": "/xdg"})
	svc, err := sys.NewService()
	if err != nil {
		t.Fatal(err)
	}
	if svc.Path() != "/xdg/systemd/user/"+setup.UnitName {
		t.Errorf("path %s", svc.Path())
	}
	unit := string(svc.Content("/opt/my apps/talaria", sys.Home+"/50%/config.yaml"))
	if !strings.Contains(unit, `ExecStart="/opt/my apps/talaria" run -c %h/50%%/config.yaml`+"\n") {
		t.Errorf("unit:\n%s", unit)
	}
}
