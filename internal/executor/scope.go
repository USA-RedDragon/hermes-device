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

// DefaultMemoryMax is the command memory cap when the daemon's own cgroup
// has no memory limit to take it from.
const DefaultMemoryMax = 4 << 30

// ErrNoScope is returned when commands cannot run in a systemd scope of
// their own.
var ErrNoScope = errors.New("commands cannot run in a systemd scope of their own")

// Scope runs each command in a transient systemd user scope of its own, with
// a memory cap, so that the kernel's OOM killer ends only a command that
// uses more than the cap, with all its processes (OOMPolicy=kill), and not
// the service that runs the daemon. The
// command keeps its process id: systemd-run --scope moves itself into the
// scope and then executes the command.
type Scope struct {
	// SystemdRun and Systemctl are the paths of the programs.
	SystemdRun, Systemctl string
	// MemoryMax is the cap of each command in bytes.
	MemoryMax int64
	// TasksMax caps the tasks of each command; 0 leaves the manager's
	// default.
	TasksMax int64
	// PartOf is the unit that runs the daemon, if any. Stopping or
	// restarting it stops the scopes too, as it stopped the commands when
	// they ran in its own cgroup.
	PartOf string
}

// argv returns the command line that runs argv in the scope unit.
func (s *Scope) argv(unit string, argv []string) []string {
	out := []string{
		s.SystemdRun, "--user", "--scope", "--quiet", "--unit=" + unit, "--description=talaria agent command",
		"-p", "MemoryMax=" + strconv.FormatInt(s.MemoryMax, 10),
		"-p", "OOMPolicy=kill",
		"-p", "TimeoutStopSec=" + strconv.Itoa(int(KillGrace/time.Second)) + "s",
	}
	if s.TasksMax > 0 {
		out = append(out, "-p", "TasksMax="+strconv.FormatInt(s.TasksMax, 10))
	}
	if s.PartOf != "" {
		out = append(out, "-p", "PartOf="+s.PartOf)
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

// NewScope returns the scope for a memory cap of memoryMax bytes, or with
// auto for 75% of the memory limit of the daemon's own cgroup
// (DefaultMemoryMax when it has none). It runs one empty command in a scope
// to check that the systemd user manager takes them. The error wraps
// ErrNoScope and says why commands cannot have a scope.
func NewScope(ctx context.Context, p ScopeProbe, memoryMax int64, auto bool) (*Scope, error) {
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
		if auto {
			if limit := readLimit(filepath.Join(dir, "memory.max")); limit > 0 {
				s.MemoryMax = limit / 4 * 3 &^ (1<<20 - 1)
			}
		}
		s.TasksMax = readLimit(filepath.Join(dir, "pids.max"))
		if base := filepath.Base(cg); strings.HasSuffix(base, ".service") && strings.Contains(cg, "/user@") {
			s.PartOf = base
		}
	}
	if s.MemoryMax <= 0 {
		s.MemoryMax = DefaultMemoryMax
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := s.argv(scopeUnit(), []string{"true"})
	probe := exec.CommandContext(pctx, argv[0], argv[1:]...) //nolint:gosec // systemd-run from PATH with fixed arguments
	probe.Env = p.Env
	var stderr bytes.Buffer
	probe.Stderr = &stderr
	if err := probe.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%w: systemd-run --user --scope failed: %s", ErrNoScope, msg)
	}
	return s, nil
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

// readLimit reads a cgroup limit file such as memory.max. It returns 0 for
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
		out, err := exec.CommandContext(ctx, s.Systemctl, "--user", "show", "--property=ActiveState,Result", unit).Output() //nolint:gosec // systemctl with a unit name we made
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
				_ = exec.CommandContext(ctx, s.Systemctl, "--user", "reset-failed", unit).Run() //nolint:gosec // as above
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
