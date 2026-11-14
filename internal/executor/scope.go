package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrNoScope is returned when commands cannot run in a systemd scope of
// their own.
var ErrNoScope = errors.New("commands cannot run in a systemd scope of their own")

// Scope runs each command in a transient systemd user scope of its own, with
// OOMPolicy=kill, so that the kernel's OOM killer ends a command that it
// kills a process of with all its processes, and not the service that runs
// the daemon. The scope has a memory cap only when MemoryMax is set. The
// command keeps its process id: systemd-run --scope moves itself into the
// scope and then executes the command.
type Scope struct {
	// SystemdRun and Systemctl are the paths of the programs.
	SystemdRun, Systemctl string
	// MemoryMax is the cap of each command in bytes; 0 sets none.
	MemoryMax int64
	// TasksMax caps the tasks of each command; 0 leaves the manager's
	// default.
	TasksMax int64
	// PartOf is the unit that runs the daemon, if any. Stopping or
	// restarting it stops the scopes too, as it stopped the commands when
	// they ran in its own cgroup.
	PartOf string
	// KeepVars passes --expand-environment=no. systemd-run 258 and later
	// expand ${NAME} in the command line of a scope by default, which
	// breaks shell parameter expansions such as ${d%/} and ${!PREFIX*};
	// systemd before 254 has no such option and never expands for a scope.
	KeepVars bool
}

// argv returns the command line that runs argv in the scope unit.
func (s *Scope) argv(unit string, argv []string) []string {
	out := []string{
		s.SystemdRun, "--user", "--scope", "--quiet", "--unit=" + unit, "--description=talaria agent command",
		"-p", "OOMPolicy=kill",
		"-p", "TimeoutStopSec=" + strconv.Itoa(int(KillGrace/time.Second)) + "s",
	}
	if s.MemoryMax > 0 {
		out = append(out, "-p", "MemoryMax="+strconv.FormatInt(s.MemoryMax, 10))
	}
	if s.TasksMax > 0 {
		out = append(out, "-p", "TasksMax="+strconv.FormatInt(s.TasksMax, 10))
	}
	if s.PartOf != "" {
		out = append(out, "-p", "PartOf="+s.PartOf)
	}
	if s.KeepVars {
		out = append(out, "--expand-environment=no")
	}
	return append(append(out, "--"), argv...)
}

func scopeUnit() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "talaria-cmd-" + hex.EncodeToString(b[:]) + ".scope"
}

// ScopeProbe is what NewScope reads. Tests replace it.
type ScopeProbe struct {
	GOOS string
	// ProcCgroup is /proc/self/cgroup and CgroupRoot the cgroup2 mount.
	ProcCgroup, CgroupRoot string
	LookPath               func(string) (string, error)
	// Env is the environment of the probe command.
	Env []string
}

// SystemProbe reads this computer.
func SystemProbe(env []string) ScopeProbe {
	return ScopeProbe{GOOS: runtime.GOOS, ProcCgroup: "/proc/self/cgroup", CgroupRoot: "/sys/fs/cgroup", LookPath: exec.LookPath, Env: env}
}

// NewScope returns the scope for a memory cap of memoryMax bytes, or with 0
// for none. It runs one empty command in a scope
// to check that the systemd user manager takes them. The error wraps
// ErrNoScope and says why commands cannot have a scope.
func NewScope(ctx context.Context, p ScopeProbe, memoryMax int64) (*Scope, error) {
	if p.GOOS != "linux" {
		return nil, fmt.Errorf("%w: only on Linux with systemd", ErrNoScope)
	}
	run, err := p.LookPath("systemd-run")
	if err != nil {
		return nil, fmt.Errorf("%w: no systemd-run: %w", ErrNoScope, err)
	}
	ctl, err := p.LookPath("systemctl")
	if err != nil {
		return nil, fmt.Errorf("%w: no systemctl: %w", ErrNoScope, err)
	}
	s := &Scope{SystemdRun: run, Systemctl: ctl, MemoryMax: memoryMax}
	if cg, ok := ownCgroup(p.ProcCgroup); ok {
		dir := filepath.Join(p.CgroupRoot, filepath.FromSlash(cg))
		s.TasksMax = readLimit(filepath.Join(dir, "pids.max"))
		if base := filepath.Base(cg); strings.HasSuffix(base, ".service") && strings.Contains(cg, "/user@") {
			s.PartOf = base
		}
	}
	s.KeepVars = true
	msg, err := s.probe(ctx, p.Env)
	if err != nil && strings.Contains(msg, "expand-environment") {
		s.KeepVars = false
		msg, err = s.probe(ctx, p.Env)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: systemd-run --user --scope failed: %s", ErrNoScope, msg)
	}
	return s, nil
}

// probe runs one empty command in a scope. On failure it returns what
// systemd-run wrote to stderr, else the error text.
func (s *Scope) probe(ctx context.Context, env []string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := s.argv(scopeUnit(), []string{"true"})
	cmd := command(pctx, s.SystemdRun, argv[1:]...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return "", nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return msg, err
}

// ownCgroup returns the cgroup v2 path of this process from the contents
// of /proc/self/cgroup at path.
func ownCgroup(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if cg, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(cg, "/") {
			return cg, true
		}
	}
	return "", false
}

// readLimit reads a cgroup limit file such as pids.max. It returns 0 for
// max and for a file it cannot read.
func readLimit(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// oomWait is how long oomKilled waits for systemd to finish a scope whose
// command it may not have stopped yet; oomSettle is how long it waits for
// one that still runs, so that systemd sees an OOM kill that ended the
// main process.
const (
	oomWait   = 3 * KillGrace
	oomSettle = 500 * time.Millisecond
)

// systemctl is the user manager's systemctl with args.
func (s *Scope) systemctl(ctx context.Context, args ...string) *exec.Cmd {
	return command(ctx, s.Systemctl, append([]string{"--user"}, args...)...)
}

// command is exec.CommandContext for a program that is configuration, not
// the caller's input, with its arguments added after it.
func command(ctx context.Context, program string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

// oomKilled reports whether systemd ended the scope unit because the
// kernel's OOM killer killed a process in it (OOMPolicy=kill gives the
// unit result oom-kill), and clears the failed unit. systemd keeps a
// failed scope until reset-failed, so the result does not race the
// unit's removal.
func (s *Scope) oomKilled(unit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), oomWait+5*time.Second)
	defer cancel()
	start := time.Now()
	for {
		out, err := s.systemctl(ctx, "show", "--property=ActiveState,Result", unit).Output()
		if err != nil {
			return false
		}
		var state, result string
		for line := range strings.SplitSeq(string(out), "\n") {
			if v, ok := strings.CutPrefix(line, "ActiveState="); ok {
				state = v
			} else if v, ok := strings.CutPrefix(line, "Result="); ok {
				result = v
			}
		}
		waited := time.Since(start)
		running := state == "active" || state == "activating" || state == "deactivating"
		if !running || state == "active" && result == "success" && waited >= oomSettle || waited >= oomWait {
			if state == "failed" {
				_ = s.systemctl(ctx, "reset-failed", unit).Run()
			}
			return result == "oom-kill"
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}
