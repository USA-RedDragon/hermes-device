//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// KillExitCode is the exit code of every process that Terminate or Kill
// ends: STATUS_CONTROL_C_EXIT, what Windows uses for a Ctrl+C.
const KillExitCode = 0xC000013A

// ntResumeProcess is ntdll's NtResumeProcess, which resumes every thread of a
// process that was started suspended.
func ntResumeProcess() *windows.LazyProc {
	return windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
}

// Tree is a Job Object that holds the command and every process it starts.
type Tree struct {
	mu  sync.Mutex
	job windows.Handle
}

// SysProcAttr starts the command suspended in a new process group, so that
// Attach can put it in the job before it runs any code.
func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
}

// Prepare adds SysProcAttr's flags to cmd, keeping its other attributes.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
}

// Attach puts a command that Prepare started suspended into a new job that
// kills its processes when the last handle to it closes (so they end with
// the daemon), then lets the command run. On an error the command is
// killed.
func Attach(cmd *exec.Cmd) (*Tree, error) {
	if cmd.Process == nil {
		return nil, errors.New("proctree: command not started")
	}
	if cmd.Process.Pid <= 0 || cmd.Process.Pid > math.MaxUint32 {
		return nil, fmt.Errorf("proctree: bad pid %d", cmd.Process.Pid)
	}
	pid := uint32(cmd.Process.Pid)
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("proctree: open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	job, err := newJob()
	if err != nil {
		_ = windows.TerminateProcess(h, KillExitCode)
		return nil, err
	}
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.TerminateProcess(h, KillExitCode)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("proctree: assign process %d to a job: %w", pid, err)
	}
	if r, _, _ := ntResumeProcess().Call(uintptr(h)); r != 0 {
		_ = windows.TerminateJobObject(job, KillExitCode)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("proctree: resume process %d: NTSTATUS 0x%x", pid, r)
	}
	return &Tree{job: job}, nil
}

func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("proctree: create job: %w", err)
	}
	if err := setKillOnClose(job, true); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func setKillOnClose(job windows.Handle, on bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if on {
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		return fmt.Errorf("proctree: set job limits: %w", err)
	}
	return nil
}

// Terminate ends every process of the tree. Windows has no polite stop for
// a process without a window, so it is Kill.
func (t *Tree) Terminate() error { return t.Kill() }

// Kill ends every process of the tree with KillExitCode.
func (t *Tree) Kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(t.job, KillExitCode)
}

// Release lets go of the job without killing what is left in it, as a
// process group's members outlive its leader on Unix. Terminate and Kill
// do nothing after it.
func (t *Tree) Release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return
	}
	_ = setKillOnClose(t.job, false)
	_ = windows.CloseHandle(t.job)
	t.job = 0
}

// Killed reports whether err is the exit of a process that Terminate or
// Kill ended.
func Killed(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		return code >= 0 && uint64(code) == KillExitCode
	}
	return false
}
