package setup_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

const (
	gitBash  = "git-bash"
	wslShell = "wsl"
	windows  = "windows"
	schtasks = "schtasks.exe"
	ps       = "powershell.exe"
)

func TestTaskService(t *testing.T) {
	t.Parallel()
	status, sid := "Ready", ""
	r := &fakeRunner{respond: func(name string, args []string) ([]byte, error) {
		if name == "whoami.exe" && sid != "" {
			return []byte(`"pc\alice","` + sid + `"` + "\r\n"), nil
		}
		if name == schtasks && args[0] == "/Query" {
			return []byte(`"\talaria","N/A","` + status + `"` + "\r\n"), nil
		}
		return nil, nil
	}}
	sys := newSystem(t, windows, r, map[string]string{schtasks: `C:\Windows\System32\schtasks.exe`},
		map[string]string{"USERNAME": "alice", "COMPUTERNAME": "PC"})
	svc, err := sys.NewService()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := svc.Available(ctx); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(sys.ConfigDir, "talaria", "talaria-task.xml"); svc.Path() != want {
		t.Errorf("Path = %s, want %s", svc.Path(), want)
	}
	content := string(svc.Content(`C:\Users\alice\.talaria\bin\talaria.exe`, `C:\Users\alice\AppData\Local\talaria\config.yaml`))
	for _, want := range []string{
		"<UserId>PC\\alice</UserId>", "<LogonType>InteractiveToken</LogonType>", "<RunLevel>LeastPrivilege</RunLevel>",
		"<LogonTrigger>", "<Command>conhost.exe</Command>",
		"<Arguments>--headless &quot;C:\\Users\\alice\\.talaria\\bin\\talaria.exe&quot; run -c &quot;C:\\Users\\alice\\AppData\\Local\\talaria\\config.yaml&quot; --service</Arguments>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "Written by talaria setup",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("task XML lacks %s:\n%s", want, content)
		}
	}
	sid = "S-1-5-21-1-2-3-1001"
	if c := string(svc.Content("a.exe", "c.yaml")); !strings.Contains(c, "<UserId>S-1-5-21-1-2-3-1001</UserId>") {
		t.Errorf("task XML does not name the SID:\n%s", c)
	}
	if svc.Active(ctx) {
		t.Error("Active with status Ready")
	}
	status = "Running"
	if !svc.Active(ctx) {
		t.Error("not Active with status Running")
	}
	writeFile(t, svc.Path(), content)
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("schtasks.exe /Create /TN talaria /XML " + filepath.Join(filepath.Dir(svc.Path()), "talaria-task-")) {
		t.Errorf("Load ran %q", r.Calls())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(svc.Path()), "*.utf16.xml")); len(left) > 0 {
		t.Errorf("Load left %q", left)
	}
	r.Reset()
	// No pid file: Restart starts the task.
	if err := svc.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("schtasks.exe /Run /TN talaria") {
		t.Errorf("Restart without a supervisor ran %q", r.Calls())
	}
	r.Reset()
	writeFile(t, filepath.Join(sys.ConfigDir, "talaria", setup.ServicePIDFile), "4242\n")
	if err := svc.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has(ps+" -NoProfile -NonInteractive -Command Stop-Process -Force -Id 4242") || r.has("schtasks.exe /Run") {
		t.Errorf("Restart with a supervisor ran %q", r.Calls())
	}
	r.Reset()
	if err := svc.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("schtasks.exe /End /TN talaria") || !r.has("schtasks.exe /Delete /TN talaria /F") {
		t.Errorf("Remove ran %q", r.Calls())
	}
}

// userPathRunner fakes the user PATH that setup reads and writes through
// PowerShell.
func userPathRunner(path *string) *fakeRunner {
	return &fakeRunner{respond: func(name string, args []string) ([]byte, error) {
		if name != ps {
			return nil, nil
		}
		cmd := args[len(args)-1]
		if strings.HasPrefix(cmd, "[Environment]::GetEnvironmentVariable") {
			return []byte(*path + "\r\n"), nil
		}
		if v, ok := strings.CutPrefix(cmd, "[Environment]::SetEnvironmentVariable('Path','"); ok {
			*path = strings.ReplaceAll(strings.TrimSuffix(v, "','User')"), "''", "'")
		}
		return nil, nil
	}}
}

func windowsOptions(t *testing.T, sys *setup.System) *setup.Options {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "talaria.exe")
	writeFile(t, exe, "binary")
	return &setup.Options{
		Version: vNew, Exe: exe, HubURL: "https://hub.example.com",
		ConfigFile: filepath.Join(sys.ConfigDir, "talaria", "config.yaml"), StateDir: filepath.Join(sys.ConfigDir, "talaria"),
		NoService: true, NoBrowser: true,
		Pair:   func(context.Context) (bool, error) { return true, nil },
		Shells: []string{gitBash, "powershell", wslShell},
	}
}

func TestWindowsSetupShellAndPath(t *testing.T) {
	t.Parallel()
	path := `C:\Users\alice\AppData\Local\Microsoft\WindowsApps`
	r := userPathRunner(&path)
	sys := newSystem(t, windows, r, nil, nil)
	out := &bytes.Buffer{}
	sys.Interactive, sys.In, sys.Out = true, strings.NewReader("2\n"), out
	o := windowsOptions(t, sys)
	rep, err := sys.Setup(context.Background(), o)
	if err != nil {
		t.Fatalf("Setup: %v\n%+v", err, rep)
	}
	bin := filepath.Join(sys.Home, setup.BinDir, "talaria.exe")
	if !exists(bin) {
		t.Errorf("no %s", bin)
	}
	if exists(filepath.Join(sys.Home, setup.BinDir, setup.AliasName)) {
		t.Error("setup linked tal on Windows")
	}
	cfg := readFile(t, o.ConfigFile)
	if !strings.Contains(cfg, "shell: powershell") || strings.Contains(cfg, "wsl-distro") {
		t.Errorf("config:\n%s", cfg)
	}
	if !strings.Contains(out.String(), "PowerShell: the agent writes PowerShell commands") {
		t.Errorf("setup did not explain the shells:\n%s", out)
	}
	dir := filepath.Dir(bin)
	if !strings.HasSuffix(path, ";"+dir) {
		t.Errorf("user PATH = %q, want %s added", path, dir)
	}
	if r.has("bash") || r.has("zsh") {
		t.Errorf("setup ran a Unix shell check on Windows: %q", r.Calls())
	}

	// A second run keeps the PATH as it is.
	before := path
	if _, err := sys.Setup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if path != before {
		t.Errorf("second run changed PATH to %q", path)
	}

	rep, err = sys.Uninstall(context.Background(), &setup.UninstallOptions{
		StateDir: o.StateDir, Unpair: func(string) error { return nil }, RemoveHost: func() error { return nil },
	})
	if err != nil {
		t.Fatalf("Uninstall: %v\n%+v", err, rep)
	}
	if path != `C:\Users\alice\AppData\Local\Microsoft\WindowsApps` {
		t.Errorf("after uninstall PATH = %q", path)
	}
}

func TestWindowsSetupShellChoice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, flag, distro string
		shells             []string
		want               string
		fails              bool
	}{
		{name: "default is the first", shells: []string{gitBash, "powershell"}, want: "shell: git-bash"},
		{name: "flag", flag: wslShell, distro: "Ubuntu", shells: []string{gitBash, wslShell}, want: "shell: wsl\n  wsl-distro: Ubuntu"},
		{name: "flag not installed", flag: wslShell, shells: []string{gitBash}, fails: true},
		{name: "nothing installed", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := ""
			sys := newSystem(t, windows, userPathRunner(&path), nil, nil)
			o := windowsOptions(t, sys)
			o.Shells, o.Shell, o.WSLDistro = tc.shells, tc.flag, tc.distro
			_, err := sys.Setup(context.Background(), o)
			if tc.fails {
				if err == nil {
					t.Fatal("Setup did not fail")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg := readFile(t, o.ConfigFile); !strings.Contains(cfg, tc.want) {
				t.Errorf("config lacks %q:\n%s", tc.want, cfg)
			}
		})
	}
}

func TestWindowsArchive(t *testing.T) {
	t.Parallel()
	if got := setup.ArchiveName("1.2.3", windows, "arm64", ""); got != "talaria_1.2.3_windows_arm64.zip" {
		t.Errorf("ArchiveName = %s", got)
	}
	if got := setup.ExeName(windows); got != "talaria.exe" {
		t.Errorf("ExeName = %s", got)
	}
	if got := setup.ExeName(linux); got != binName {
		t.Errorf("ExeName(linux) = %s", got)
	}
}
