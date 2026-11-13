//go:build unix

package launch_test

import (
	"slices"
	"testing"
)

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
