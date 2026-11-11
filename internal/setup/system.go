// Package setup installs hermes-device for a desktop user: the binary at a
// stable path, the config file, the background service, Firefox's native
// messaging host and extension, and a shell environment that agent commands
// share. Uninstall reverses what setup recorded in its state file.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs the programs setup needs: systemctl, launchctl, shells and
// Firefox. Tests replace it.
type Runner interface {
	// Output runs name with args and env (nil: the current environment)
	// and returns its stdout. stdin is empty.
	Output(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
	// Start starts name with args without waiting for it.
	Start(name string, args ...string) error
}

// System is the computer setup works on. Tests fill it with a temporary
// home and a fake Runner.
type System struct {
	GOOS string
	// Home is the user's home directory.
	Home string
	// ConfigDir is the user config directory (~/.config on Linux).
	ConfigDir string
	// CacheDir is the user cache directory (~/.cache on Linux).
	CacheDir string
	// UID is the user's id, for launchctl's gui domain.
	UID int
	// Shell is the user's login shell.
	Shell  string
	Run    Runner
	Getenv func(string) string
	// LookPath finds a program on PATH.
	LookPath func(string) (string, error)
	// In answers prompts; Interactive is true when it is a terminal.
	In          io.Reader
	Interactive bool
	Out         io.Writer
	// Now is the clock for backup file names.
	Now func() time.Time
}

// OSRunner runs real programs.
type OSRunner struct{}

// Output implements Runner.
func (OSRunner) Output(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Env = env
	// A child that keeps stdout open must not hold setup past ctx.
	c.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// Start implements Runner.
func (OSRunner) Start(name string, args ...string) error {
	c := exec.Command(name, args...) //nolint:noctx // the program outlives setup
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

const darwin = "darwin"

// ErrNoHome is returned when the home directory is unknown or is /.
var ErrNoHome = errors.New("setup needs a home directory other than /")

// LocalSystem describes this computer.
func LocalSystem(goos string) (*System, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return nil, ErrNoHome
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}
	return &System{
		GOOS: goos, Home: home, ConfigDir: cfg, CacheDir: cache, UID: os.Getuid(),
		Shell: loginShell(os.Getenv("SHELL"), os.Getuid()), Run: OSRunner{}, Getenv: os.Getenv,
		LookPath: exec.LookPath, In: os.Stdin, Interactive: interactive, Out: os.Stdout, Now: time.Now,
	}, nil
}

// loginShell is $SHELL, else the shell /etc/passwd gives uid.
func loginShell(env string, uid int) string {
	if env != "" {
		return env
	}
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	return passwdShell(string(data), uid)
}

func passwdShell(passwd string, uid int) string {
	want := fmt.Sprint(uid)
	for line := range strings.SplitSeq(passwd, "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[2] == want {
			return f[6]
		}
	}
	return ""
}

// hasDesktop reports whether programs setup starts can open windows.
func (s *System) hasDesktop() bool {
	return s.GOOS == darwin || s.Getenv("DISPLAY") != "" || s.Getenv("WAYLAND_DISPLAY") != ""
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (s *System) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.Out, format, args...)
}
