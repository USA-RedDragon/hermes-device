package setup

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// TaskName is the Windows Scheduled Task that runs talaria run at logon.
const TaskName = "talaria"

// ServiceEnv is set to ServiceTask in the environment of the daemon that
// the scheduled task's supervisor starts.
const (
	ServiceEnv  = "TALARIA_SERVICE"
	ServiceTask = "task"
)

// ServicePIDFile, in talaria's config directory, holds the pid of the
// daemon that the scheduled task's supervisor runs, so that Restart can
// stop it and the supervisor starts the binary again.
const ServicePIDFile = "service.pid"

// ServiceLogFile, in talaria's config directory, is the daemon's log
// under the scheduled task.
const ServiceLogFile = "talaria.log"

// taskService is a per-user Scheduled Task with a logon trigger: no
// administrator rights needed. Its action runs talaria run --service in a
// console without a window; that supervisor restarts the daemon when it
// exits, as systemd's Restart=always does.
type taskService struct{ s *System }

func (t *taskService) dir() string { return filepath.Join(t.s.ConfigDir, BinaryName) }

func (t *taskService) Path() string { return filepath.Join(t.dir(), "talaria-task.xml") }

func (t *taskService) Name() string { return "scheduled task " + TaskName }

func (t *taskService) schtasks(ctx context.Context, args ...string) error {
	out, err := t.s.Run.Output(ctx, nil, "schtasks.exe", args...)
	if err != nil {
		return fmt.Errorf("schtasks %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *taskService) Available(context.Context) error {
	if _, err := t.s.LookPath("schtasks.exe"); err != nil {
		return ErrNoServiceManager
	}
	return nil
}

// user is the SID of the user that setup runs as, which the task's
// trigger and principal name. Without whoami it is COMPUTERNAME\USERNAME;
// USERDOMAIN is no help, as an ssh session sets it to WORKGROUP.
func (t *taskService) user() string {
	out, err := t.s.Run.Output(context.Background(), nil, "whoami.exe", "/user", "/fo", "csv", "/nh")
	if err == nil {
		recs, cerr := csv.NewReader(bytes.NewReader(bytes.TrimSpace(out))).ReadAll()
		if cerr == nil && len(recs) == 1 && len(recs[0]) == 2 && strings.HasPrefix(recs[0][1], "S-1-") {
			return recs[0][1]
		}
	}
	name := t.s.Getenv("USERNAME")
	if c := t.s.Getenv("COMPUTERNAME"); c != "" {
		return c + `\` + name
	}
	return name
}

func quoteArg(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func (t *taskService) Content(bin, cfg string) []byte {
	user := xmlEscape(t.user())
	args := xmlEscape("--headless " + quoteArg(bin) + " run -c " + quoteArg(cfg) + " --service")
	// The file is kept in UTF-8; Load hands schtasks a UTF-16 copy, which
	// the declaration describes. A comment may not hold "--", so the
	// marker is in the description.
	return []byte(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>talaria: runs the Hermes agent's commands on this PC. ` + serviceMarker + `; talaria setup --uninstall removes it.</Description>
    <URI>\` + TaskName + `</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + user + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + user + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>conhost.exe</Command>
      <Arguments>` + args + `</Arguments>
    </Exec>
  </Actions>
</Task>
`)
}

// Load registers the task from its file. schtasks reads the file as
// UTF-16, so Load hands it a UTF-16 copy.
func (t *taskService) Load(ctx context.Context) error {
	data, err := os.ReadFile(t.Path())
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(t.dir(), "talaria-task-*.utf16.xml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(utf16LE(data))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return t.schtasks(ctx, "/Create", "/TN", TaskName, "/XML", tmp.Name(), "/F")
}

// utf16LE encodes UTF-8 text as UTF-16LE with a byte order mark.
func utf16LE(data []byte) []byte {
	out := []byte{0xff, 0xfe}
	for _, u := range utf16.Encode([]rune(string(data))) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

func (t *taskService) LoadStarts() bool { return false }

// status is the task's Status column: Ready, Running or Disabled, or ""
// when the task does not exist.
func (t *taskService) status(ctx context.Context) string {
	out, err := t.s.Run.Output(ctx, nil, "schtasks.exe", "/Query", "/TN", TaskName, "/FO", "CSV", "/NH")
	if err != nil {
		return ""
	}
	recs, err := csv.NewReader(bytes.NewReader(bytes.TrimSpace(out))).ReadAll()
	if err != nil || len(recs) == 0 || len(recs[0]) < 3 {
		return ""
	}
	return strings.TrimSpace(recs[0][2])
}

func (t *taskService) Active(ctx context.Context) bool { return t.status(ctx) == "Running" }

func (t *taskService) Start(ctx context.Context) error {
	err := t.schtasks(ctx, "/Run", "/TN", TaskName)
	return err
}

// Restart stops the daemon that the task's supervisor runs; the supervisor
// starts the binary again after its crash delay (5 s). This also works from a command that
// runs inside the service, such as talaria update run by the agent, where
// ending the task would end the caller too. Without a running supervisor
// it starts the task.
func (t *taskService) Restart(ctx context.Context) error {
	data, err := os.ReadFile(filepath.Join(t.dir(), ServicePIDFile))
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || perr != nil || pid <= 0 || !t.Active(ctx) {
		return t.Start(ctx)
	}
	// Not taskkill: it gets "Access denied" for a daemon that runs in the
	// desktop session when update runs over SSH; Stop-Process does not.
	if _, err := t.s.Run.Output(ctx, nil, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"Stop-Process -Force -Id "+strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("stop the daemon (pid %d): %w", pid, err)
	}
	return nil
}

func (t *taskService) Remove(ctx context.Context) error {
	if t.status(ctx) == "" {
		return nil
	}
	_, _ = t.s.Run.Output(ctx, nil, "schtasks.exe", "/End", "/TN", TaskName)
	err := t.schtasks(ctx, "/Delete", "/TN", TaskName, "/F")
	return err
}

func (t *taskService) Logs() string {
	return "Get-Content -Wait -Tail 50 " + quoteArg(filepath.Join(t.dir(), ServiceLogFile))
}
