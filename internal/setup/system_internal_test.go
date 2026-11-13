package setup

import (
	"os"
	"testing"
)

// /dev/null is a character device but no terminal: setup run with
// stdin from it (launchd, cron, ssh host cmd < /dev/null) must not prompt.
func TestIsTerminalDevNull(t *testing.T) {
	t.Parallel()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("isTerminal(%s) = true", os.DevNull)
	}
	p, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if isTerminal(p) {
		t.Error("isTerminal(a plain file) = true")
	}
}

func TestHasDesktop(t *testing.T) {
	t.Parallel()
	const linux = "linux"
	for _, tc := range []struct {
		goos string
		env  map[string]string
		want bool
	}{
		{darwin, nil, true},
		{darwin, map[string]string{"SSH_CONNECTION": "192.0.2.1 50000 192.0.2.2 22"}, false},
		{darwin, map[string]string{"SSH_TTY": "/dev/ttys001"}, false},
		{linux, map[string]string{"DISPLAY": ":0"}, true},
		{linux, map[string]string{"WAYLAND_DISPLAY": "wayland-0", "SSH_CONNECTION": "x"}, true},
		{linux, nil, false},
	} {
		s := &System{GOOS: tc.goos, Getenv: func(k string) string { return tc.env[k] }}
		if got := s.HasDesktop(); got != tc.want {
			t.Errorf("%s %v: HasDesktop = %v, want %v", tc.goos, tc.env, got, tc.want)
		}
	}
}
