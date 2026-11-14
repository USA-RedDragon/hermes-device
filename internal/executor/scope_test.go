//go:build unix

package executor_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/executor"
)

// fakeSystemd writes a systemd-run that records its arguments, one per
// line, appending a line "END" per call, and then runs the command after
// --, and a systemctl that prints show. It returns the directory and the
// arguments file.
func fakeSystemd(t *testing.T, runExit, show string) (dir, args string) {
	t.Helper()
	dir = t.TempDir()
	args = filepath.Join(dir, "args")
	run := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + args + "'; done\necho END >> '" + args + "'\n" + runExit +
		"while [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
	ctl := "#!/bin/sh\necho \"$*\" >> '" + filepath.Join(dir, "ctl") + "'\ncase \"$*\" in *show*) printf '" + show + "' ;; esac\n"
	for name, body := range map[string]string{"systemd-run": run, "systemctl": ctl} {
		// Hold ForkLock so that no parallel test forks a child that keeps
		// the file open for writing, which makes its exec fail with
		// ETXTBSY (go.dev/issue/22315).
		syscall.ForkLock.RLock()
		prog := filepath.Join(dir, name)
		err := os.WriteFile(prog, []byte(body), 0o600)
		if err == nil {
			err = os.Chmod(prog, 0o700)
		}
		syscall.ForkLock.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir, args
}

// probe returns a ScopeProbe whose cgroup is cg, with memory.max and
// pids.max as given, and whose programs are in bin.
func probe(t *testing.T, bin, cg, memMax, pidsMax string) executor.ScopeProbe {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash(cg))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"memory.max": memMax, "pids.max": pidsMax} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proc := filepath.Join(root, "cgroup")
	if err := os.WriteFile(proc, []byte("0::"+cg+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return executor.ScopeProbe{
		GOOS: "linux", ProcCgroup: proc, CgroupRoot: root, Env: os.Environ(),
		LookPath: func(name string) (string, error) {
			p := filepath.Join(bin, name)
			if _, err := os.Stat(p); err != nil {
				return "", err
			}
			return p, nil
		},
	}
}

const serviceCgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/talaria.service"

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestNewScopeNoCapByDefault(t *testing.T) {
	t.Parallel()
	bin, args := fakeSystemd(t, "", "")
	s, err := executor.NewScope(t.Context(), probe(t, bin, serviceCgroup, "2147483648", "512"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryMax != 0 || s.TasksMax != 512 || s.PartOf != "talaria.service" {
		t.Errorf("scope = %+v", s)
	}
	got := strings.Join(readArgs(t, args), " ")
	for _, want := range []string{
		"--user --scope --quiet --unit=talaria-cmd-", "-p OOMPolicy=kill",
		"-p TasksMax=512", "-p PartOf=talaria.service", "--expand-environment=no -- true END",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("probe args %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "MemoryMax") {
		t.Errorf("probe args %q set a MemoryMax", got)
	}
}

func TestNewScopeDefaults(t *testing.T) {
	t.Parallel()
	bin, _ := fakeSystemd(t, "", "")
	s, err := executor.NewScope(t.Context(), probe(t, bin, "/user.slice/user-1000.slice/session-4.scope", "max", "max"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryMax != 0 || s.TasksMax != 0 || s.PartOf != "" {
		t.Errorf("scope = %+v", s)
	}
	s, err = executor.NewScope(t.Context(), probe(t, bin, serviceCgroup, "2147483648", "512"), 3<<30)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryMax != 3<<30 {
		t.Errorf("explicit cap became %d", s.MemoryMax)
	}
}

func TestNewScopeUnavailable(t *testing.T) {
	t.Parallel()
	bin, _ := fakeSystemd(t, "", "")
	failing, _ := fakeSystemd(t, "echo 'Failed to connect to bus: No medium found' >&2\nexit 1\n", "")
	darwin := probe(t, bin, serviceCgroup, "max", "max")
	darwin.GOOS = "darwin"
	missing := probe(t, t.TempDir(), serviceCgroup, "max", "max")
	for name, c := range map[string]struct {
		p    executor.ScopeProbe
		want string
	}{
		"darwin":     {darwin, "only on Linux"},
		"no program": {missing, "no systemd-run"},
		"no bus":     {probe(t, failing, serviceCgroup, "max", "max"), "Failed to connect to bus"},
	} {
		s, err := executor.NewScope(t.Context(), c.p, 0)
		if !errors.Is(err, executor.ErrNoScope) || !strings.Contains(err.Error(), c.want) || s != nil {
			t.Errorf("%s: %v, %v", name, s, err)
		}
	}
}

func TestNewScopeWithoutExpandOption(t *testing.T) {
	t.Parallel()
	old := "case \"$*\" in *--expand-environment*) echo \"systemd-run: unrecognized option '--expand-environment=no'\" >&2; exit 1 ;; esac\n"
	bin, args := fakeSystemd(t, old, "")
	s, err := executor.NewScope(t.Context(), probe(t, bin, serviceCgroup, "max", "max"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.KeepVars {
		t.Error("kept --expand-environment=no that systemd-run does not know")
	}
	got := strings.Join(readArgs(t, args), " ")
	if strings.Count(got, "END") != 2 || !strings.HasSuffix(got, "talaria.service -- true END") {
		t.Errorf("probe args %q", got)
	}
}

func TestStartInScope(t *testing.T) {
	t.Parallel()
	bin, args := fakeSystemd(t, "", "")
	s := &executor.Scope{SystemdRun: filepath.Join(bin, "systemd-run"), Systemctl: filepath.Join(bin, "systemctl"), MemoryMax: 64 << 20}
	res, out := run(t, executor.Request{Cmd: "echo hi; exit 3", Cwd: t.TempDir(), Login: true, Env: os.Environ(), Scope: s})
	if res.Code != 3 || out != "hi\n" {
		t.Errorf("result %+v, output %q", res, out)
	}
	got := strings.Join(readArgs(t, args), " ")
	if !strings.Contains(got, "-p MemoryMax=67108864") || !strings.HasSuffix(got, "-- bash -l -c echo hi; exit 3 END") || strings.Contains(got, "TasksMax") {
		t.Errorf("args %q", got)
	}
}

func TestStartInScopeKeepsVars(t *testing.T) {
	t.Parallel()
	bin, args := fakeSystemd(t, "", "")
	s := scopeIn(bin)
	s.KeepVars = true
	cmd := `d=/home/alice/; echo "${d%/}/x" ${!HOME*}`
	res, out := run(t, executor.Request{Cmd: cmd, Cwd: t.TempDir(), Env: []string{"HOME=/home/alice", "PATH=" + os.Getenv("PATH")}, Scope: s})
	if res.Code != 0 || out != "/home/alice/x HOME\n" {
		t.Errorf("result %+v, output %q", res, out)
	}
	if got := strings.Join(readArgs(t, args), " "); !strings.HasSuffix(got, "--expand-environment=no -- bash -c "+cmd+" END") {
		t.Errorf("args %q", got)
	}
}

func scopeIn(bin string) *executor.Scope {
	return &executor.Scope{SystemdRun: filepath.Join(bin, "systemd-run"), Systemctl: filepath.Join(bin, "systemctl"), MemoryMax: 64 << 20}
}

func TestScopeReportsOOM(t *testing.T) {
	t.Parallel()
	bin, args := fakeSystemd(t, "", `ActiveState=failed\nResult=oom-kill\n`)
	res, _ := run(t, executor.Request{Cmd: "kill -KILL $$", Cwd: t.TempDir(), Env: os.Environ(), Scope: scopeIn(bin)})
	if !res.OOM || res.MemoryMax != 64<<20 || res.Signal != syscall.SIGKILL || res.Code != 137 || res.Killed {
		t.Errorf("result %+v", res)
	}
	var unit string
	for _, a := range readArgs(t, args) {
		if u, ok := strings.CutPrefix(a, "--unit="); ok {
			unit = u
		}
	}
	ctl, _ := os.ReadFile(filepath.Join(bin, "ctl"))
	if unit == "" || string(ctl) != "--user show --property=ActiveState,Result "+unit+"\n--user reset-failed "+unit+"\n" {
		t.Errorf("systemctl calls %q for unit %q", ctl, unit)
	}
}

func TestScopeReportsOOMWithoutCap(t *testing.T) {
	t.Parallel()
	bin, args := fakeSystemd(t, "", `ActiveState=failed\nResult=oom-kill\n`)
	s := scopeIn(bin)
	s.MemoryMax = 0
	res, _ := run(t, executor.Request{Cmd: "kill -KILL $$", Cwd: t.TempDir(), Env: os.Environ(), Scope: s})
	if !res.OOM || res.MemoryMax != 0 || res.Code != 137 {
		t.Errorf("result %+v", res)
	}
	if got := strings.Join(readArgs(t, args), " "); strings.Contains(got, "MemoryMax") || !strings.Contains(got, "-p OOMPolicy=kill") {
		t.Errorf("args %q", got)
	}
}

func TestScopeSignalWithoutOOM(t *testing.T) {
	t.Parallel()
	bin, _ := fakeSystemd(t, "", `ActiveState=inactive\nResult=success\n`)
	res, _ := run(t, executor.Request{Cmd: "kill -KILL $$", Cwd: t.TempDir(), Env: os.Environ(), Scope: scopeIn(bin)})
	if res.OOM || res.Signal != syscall.SIGKILL || res.Code != 137 {
		t.Errorf("result %+v", res)
	}
	if ctl, _ := os.ReadFile(filepath.Join(bin, "ctl")); strings.Contains(string(ctl), "reset-failed") {
		t.Errorf("reset a unit that did not fail: %q", ctl)
	}
}

func TestScopeExitSkipsSystemctl(t *testing.T) {
	t.Parallel()
	bin, _ := fakeSystemd(t, "", `ActiveState=failed\nResult=oom-kill\n`)
	res, _ := run(t, executor.Request{Cmd: "exit 137", Cwd: t.TempDir(), Env: os.Environ(), Scope: scopeIn(bin)})
	if res.OOM || res.Signal != 0 || res.Code != 137 {
		t.Errorf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(bin, "ctl")); err == nil {
		t.Error("asked systemctl about a command that exited")
	}
}

// TestScopeOOMSystemd runs a command that allocates 256 MiB in a real
// systemd user scope capped at 64 MiB. It needs a systemd user manager,
// python3, and TALARIA_SYSTEMD_TESTS=1.
func TestScopeOOMSystemd(t *testing.T) {
	t.Parallel()
	if os.Getenv("TALARIA_SYSTEMD_TESTS") != "1" {
		t.Skip("set TALARIA_SYSTEMD_TESTS=1 to run commands in real systemd scopes")
	}
	s, err := executor.NewScope(t.Context(), executor.SystemProbe(os.Environ()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	cmd := `python3 -c 'b = bytearray(256 << 20); print("survived")'; echo after`
	res, out := run(t, executor.Request{Cmd: cmd, Cwd: t.TempDir(), Env: os.Environ(), Scope: s})
	if !res.OOM || res.MemoryMax != 64<<20 || res.Signal == 0 || strings.Contains(out, "survived") || strings.Contains(out, "after") {
		t.Errorf("result %+v, output %q", res, out)
	}
	res, out = run(t, executor.Request{Cmd: "echo small; exit 4", Cwd: t.TempDir(), Env: os.Environ(), Scope: s})
	if res.OOM || res.Code != 4 || out != "small\n" {
		t.Errorf("small command: %+v, %q", res, out)
	}
}

// TestScopeKeepsShellExpansionsSystemd checks that systemd-run passes
// ${...} to the shell as written. It needs a systemd user manager and
// TALARIA_SYSTEMD_TESTS=1.
func TestScopeKeepsShellExpansionsSystemd(t *testing.T) {
	t.Parallel()
	if os.Getenv("TALARIA_SYSTEMD_TESTS") != "1" {
		t.Skip("set TALARIA_SYSTEMD_TESTS=1 to run commands in real systemd scopes")
	}
	s, err := executor.NewScope(t.Context(), executor.SystemProbe(os.Environ()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	cmd := `d=/home/alice/; TALARIA_T_A=1; echo "${d%/}/x" ${!TALARIA_T_*} "${TALARIA_T_B:+set}"`
	res, out := run(t, executor.Request{Cmd: cmd, Cwd: t.TempDir(), Env: append(os.Environ(), "TALARIA_T_B=1"), Scope: s})
	if res.Code != 0 || out != "/home/alice/x TALARIA_T_A TALARIA_T_B set\n" {
		t.Errorf("result %+v, output %q", res, out)
	}
}
