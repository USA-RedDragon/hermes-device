package launch_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/browser/launch"
)

func fake(environ []string, manager string, exists ...string) *launch.Launcher {
	return &launch.Launcher{
		Command: "firefox",
		GOOS:    "linux",
		UID:     1000,
		Environ: func() []string { return environ },
		Output: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "systemctl" || strings.Join(args, " ") != "--user show-environment" {
				return nil, errors.New("unexpected " + name)
			}
			if manager == "" {
				return nil, errors.New("no user manager")
			}
			return []byte(manager), nil
		},
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Exists:   func(p string) bool { return slices.Contains(exists, p) },
	}
}

func TestEnvFromUserManager(t *testing.T) {
	t.Parallel()
	l := fake([]string{"HOME=/home/alice", "PATH=/usr/bin"},
		"HOME=/home/alice\nWAYLAND_DISPLAY=wayland-0\nDISPLAY=:1\nXDG_RUNTIME_DIR=/run/user/1000\n"+
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus\nXDG_CURRENT_DESKTOP=KDE\nWEIRD=$'a\\nb'\nOTHER=x\n",
		"/run/user/1000/wayland-0")
	env, err := l.Env(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/home/alice", "PATH=/usr/bin", "WAYLAND_DISPLAY=wayland-0", "DISPLAY=:1",
		"XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "XDG_CURRENT_DESKTOP=KDE"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %q\nwant  %q", env, want)
	}
}

func TestEnvOwnDisplayWins(t *testing.T) {
	t.Parallel()
	l := fake([]string{"DISPLAY=:0"}, "WAYLAND_DISPLAY=wayland-9\n", "/tmp/.X11-unix/X0", "/run/user/1000", "/run/user/1000/bus")
	env, err := l.Env(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DISPLAY=:0", "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
}

func TestNoSession(t *testing.T) {
	t.Parallel()
	const home = "HOME=/h"
	for name, l := range map[string]*launch.Launcher{
		"none":   fake([]string{home}, home+"\n"),
		"no bus": fake([]string{home}, ""),
		"stale":  fake([]string{home}, "WAYLAND_DISPLAY=wayland-0\nXDG_RUNTIME_DIR=/run/user/1000\n"),
	} {
		if _, err := l.Env(t.Context()); !errors.Is(err, launch.ErrNoSession) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err := l.Launch(t.Context()); !errors.Is(err, launch.ErrNoSession) {
			t.Errorf("%s: Launch err = %v", name, err)
		}
	}
}

func TestArgv(t *testing.T) {
	t.Parallel()
	l := fake(nil, "")
	argv, err := l.Argv()
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/systemd-run" || !slices.Contains(argv, "--scope") || !slices.Contains(argv, "--user") ||
		argv[len(argv)-1] != "/usr/bin/firefox" || !strings.HasPrefix(argv[5], "--unit=app-hermes-firefox-") {
		t.Errorf("argv = %q", argv)
	}
	l.LookPath = func(name string) (string, error) {
		if name == "systemd-run" {
			return "", errors.New("not found")
		}
		return "/opt/firefox/firefox", nil
	}
	if argv, _ := l.Argv(); !slices.Equal(argv, []string{"/opt/firefox/firefox"}) {
		t.Errorf("argv without systemd-run = %q", argv)
	}
	l.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := l.Argv(); err == nil || !strings.Contains(err.Error(), "browser.firefox-command") {
		t.Errorf("missing command: err = %v", err)
	}
	l.GOOS = "darwin"
	if argv, _ := l.Argv(); !slices.Equal(argv, []string{"open", "-a", "Firefox"}) {
		t.Errorf("darwin argv = %q", argv)
	}
}

func TestScreenLocked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, display, hint string
		want                launch.Lock
	}{
		{"locked", "2\n", "yes\n", launch.Lock{Known: true, Locked: true, Source: "logind LockedHint of session 2"}},
		{"unlocked", "2", "no", launch.Lock{Known: true, Source: "logind LockedHint of session 2"}},
		{"no session", "", "", launch.Lock{Source: "loginctl found no graphical session for this user"}},
	} {
		l := &launch.Launcher{GOOS: "linux", UID: 1000, Output: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch strings.Join(append([]string{name}, args...), " ") {
			case "loginctl show-user 1000 -p Display --value":
				return []byte(tc.display), nil
			case "loginctl show-session 2 -p LockedHint --value":
				return []byte(tc.hint), nil
			}
			return nil, errors.New("unexpected")
		}}
		if got := l.ScreenLocked(t.Context()); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := (&launch.Launcher{GOOS: "darwin"}).ScreenLocked(t.Context()); got.Known {
		t.Errorf("darwin: %+v", got)
	}
}
