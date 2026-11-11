// Package launch starts the user's Firefox in the user's graphical session
// for hermes-device run, which usually runs as a systemd user service
// without the session's display variables (docs/browser.md section 5.1).
package launch

import (
	"bufio"
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
	"sync"
	"syscall"
	"time"
)

// ErrNoSession means no graphical session was found for Firefox.
var ErrNoSession = errors.New("no graphical session")

// sessionKeys are the variables a desktop program takes from the user's
// session.
var sessionKeys = []string{ //nolint:gochecknoglobals // fixed list
	"WAYLAND_DISPLAY", "DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "XDG_SESSION_TYPE",
	"XDG_CURRENT_DESKTOP", "XDG_SESSION_DESKTOP", "DESKTOP_SESSION", "XDG_DATA_DIRS", "XDG_CONFIG_DIRS",
	"MOZ_ENABLE_WAYLAND",
}

// earlyExit is how long Launch waits to see whether the launcher fails at
// once.
const earlyExit = 2 * time.Second

// Launcher starts Firefox. The function fields default to the real ones;
// tests replace them.
type Launcher struct {
	// Command starts Firefox: a name on PATH or an absolute path.
	Command string
	// Output runs a helper command and returns its standard output.
	Output func(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath finds a command on PATH.
	LookPath func(name string) (string, error)
	// Environ is the daemon's own environment.
	Environ func() []string
	// Exists reports whether a file (a socket, a directory) exists.
	Exists func(path string) bool
	// GOOS is the operating system.
	GOOS string
	// UID is the user id, for /run/user/<uid>.
	UID int

	mu sync.Mutex
}

// New returns a Launcher for command.
func New(command string) *Launcher {
	return &Launcher{Command: command}
}

func (l *Launcher) defaults() {
	if l.Command == "" {
		l.Command = "firefox"
	}
	if l.Output == nil {
		l.Output = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed helper commands
		}
	}
	if l.LookPath == nil {
		l.LookPath = exec.LookPath
	}
	if l.Environ == nil {
		l.Environ = os.Environ
	}
	if l.Exists == nil {
		l.Exists = func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		}
	}
	if l.GOOS == "" {
		l.GOOS = runtime.GOOS
		l.UID = os.Getuid()
	}
}

// parseEnv reads KEY=VALUE lines, as systemctl --user show-environment
// prints them. Values systemd had to quote ($'...') are skipped.
func parseEnv(out []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || k == "" || strings.HasPrefix(v, "$'") {
			continue
		}
		m[k] = v
	}
	return m
}

// Env is the environment Firefox starts with: the daemon's own, plus the
// session variables of the systemd user manager that the daemon lacks
// (desktops import them there at login). It fails with ErrNoSession when
// neither has a display whose socket exists.
func (l *Launcher) Env(ctx context.Context) ([]string, error) {
	l.defaults()
	env := map[string]string{}
	var order []string
	for _, kv := range l.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, dup := env[k]; !dup {
			order = append(order, k)
		}
		env[k] = v
	}
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	if l.GOOS == "darwin" {
		return join(env, order), nil
	}
	if env["WAYLAND_DISPLAY"] == "" && env["DISPLAY"] == "" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := l.Output(cctx, "systemctl", "--user", "show-environment")
		cancel()
		if err == nil {
			mgr := parseEnv(out)
			for _, k := range sessionKeys {
				if v := mgr[k]; v != "" && env[k] == "" {
					set(k, v)
				}
			}
		}
	}
	if env["XDG_RUNTIME_DIR"] == "" {
		if dir := fmt.Sprintf("/run/user/%d", l.UID); l.Exists(dir) {
			set("XDG_RUNTIME_DIR", dir)
		}
	}
	if env["DBUS_SESSION_BUS_ADDRESS"] == "" && env["XDG_RUNTIME_DIR"] != "" {
		if bus := filepath.Join(env["XDG_RUNTIME_DIR"], "bus"); l.Exists(bus) {
			set("DBUS_SESSION_BUS_ADDRESS", "unix:path="+bus)
		}
	}
	wayland := env["WAYLAND_DISPLAY"]
	if wayland != "" && !filepath.IsAbs(wayland) {
		wayland = filepath.Join(env["XDG_RUNTIME_DIR"], wayland)
	}
	waylandOK := env["WAYLAND_DISPLAY"] != "" && l.Exists(wayland)
	xOK := false
	if d := env["DISPLAY"]; d != "" {
		// A local display :N has the socket /tmp/.X11-unix/XN; a remote one
		// (host:N) cannot be checked here.
		if num, ok := strings.CutPrefix(d, ":"); ok {
			num, _, _ = strings.Cut(num, ".")
			xOK = l.Exists("/tmp/.X11-unix/X" + num)
		} else {
			xOK = true
		}
	}
	switch {
	case waylandOK:
	case xOK:
		delete(env, "WAYLAND_DISPLAY")
	case env["WAYLAND_DISPLAY"] == "" && env["DISPLAY"] == "":
		return nil, fmt.Errorf("%w: neither hermes-device nor the systemd user manager has WAYLAND_DISPLAY or DISPLAY "+
			"set. Sign in to the desktop on this computer; Firefox needs a display", ErrNoSession)
	default:
		return nil, fmt.Errorf("%w: the display in WAYLAND_DISPLAY=%q DISPLAY=%q has no socket, so the desktop "+
			"session probably ended. Sign in to the desktop on this computer", ErrNoSession, env["WAYLAND_DISPLAY"], env["DISPLAY"])
	}
	return join(env, order), nil
}

func join(env map[string]string, order []string) []string {
	out := make([]string, 0, len(order))
	for _, k := range order {
		if v, ok := env[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Argv is the command line that starts Firefox. On Linux with systemd it
// runs in a scope of its own under the user manager, so stopping
// hermes-device run does not stop Firefox.
func (l *Launcher) Argv() ([]string, error) {
	l.defaults()
	if l.GOOS == "darwin" && !filepath.IsAbs(l.Command) {
		return []string{"open", "-a", "Firefox"}, nil
	}
	path, err := l.LookPath(l.Command)
	if err != nil {
		return nil, fmt.Errorf("browser.firefox-command %q: %w", l.Command, err)
	}
	if l.GOOS != "linux" {
		return []string{path}, nil
	}
	run, lookErr := l.LookPath("systemd-run")
	if lookErr != nil {
		return []string{path}, nil //nolint:nilerr // without systemd-run, start Firefox directly
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return []string{run, "--user", "--scope", "--collect", "--quiet", "--unit=app-hermes-firefox-" + hex.EncodeToString(b[:]) + ".scope",
		"--description=Firefox started by hermes-device", "--", path}, nil
}

// Launch starts Firefox with the user's default profile and returns once
// it runs. Firefox outlives ctx. A Firefox that already runs opens a
// window and the launcher exits; that counts as started.
func (l *Launcher) Launch(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	env, err := l.Env(ctx)
	if err != nil {
		return err
	}
	argv, err := l.Argv()
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // configured command; must outlive ctx
	cmd.Env = env
	// Standard streams stay on /dev/null: a pipe would break when the
	// daemon exits while Firefox runs on.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s exited at once: %w", filepath.Base(argv[0]), err)
		}
	case <-time.After(earlyExit):
	case <-ctx.Done():
	}
	return nil
}

// Lock is what ScreenLocked found out about the user's screen lock.
type Lock struct {
	// Known is false when it could not be found out.
	Known bool `json:"known"`
	// Locked is logind's LockedHint of the user's graphical session.
	Locked bool `json:"locked"`
	// Source says where it came from, or why it is unknown.
	Source string `json:"source"`
}

// ScreenLocked reports whether the user's graphical session is locked, from
// logind's LockedHint for the user's display session. Desktops such as KDE
// Plasma and GNOME set it when the screen locks. It never starts anything.
func (l *Launcher) ScreenLocked(ctx context.Context) Lock {
	l.mu.Lock()
	l.defaults()
	l.mu.Unlock()
	if l.GOOS != "linux" {
		return Lock{Source: "not known on " + l.GOOS}
	}
	out, err := l.Output(ctx, "loginctl", "show-user", strconv.Itoa(l.UID), "-p", "Display", "--value")
	session := strings.TrimSpace(string(out))
	if err != nil || session == "" {
		return Lock{Source: "loginctl found no graphical session for this user"}
	}
	out, err = l.Output(ctx, "loginctl", "show-session", session, "-p", "LockedHint", "--value")
	switch v := strings.TrimSpace(string(out)); {
	case err != nil:
		return Lock{Source: "loginctl show-session failed: " + err.Error()}
	case v == "yes" || v == "no":
		return Lock{Known: true, Locked: v == "yes", Source: "logind LockedHint of session " + session}
	default:
		return Lock{Source: "loginctl gave no LockedHint for session " + session}
	}
}
