package setup_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/setup"
)

var errNotFound = errors.New("not found")

const (
	systemctl    = "systemctl"
	launchctl    = "launchctl"
	binLaunchctl = "/bin/launchctl"
	lcPrint      = "print"
	linux        = "linux"
	vNew         = "0.9.0"
	vOld         = "0.8.1"
	binName      = "talaria"
	zshrcText    = "export EDITOR=vi\n"
)

// fakeRunner answers commands with respond and records them.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	started []string
	respond func(name string, args []string) ([]byte, error)
}

func (f *fakeRunner) Output(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(append([]string{filepath.Base(name)}, args...), " "))
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return nil, nil
	}
	return respond(filepath.Base(name), args)
}

func (f *fakeRunner) Start(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, strings.Join(append([]string{name}, args...), " "))
	return nil
}

func (f *fakeRunner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.started = nil, nil
}

func (f *fakeRunner) has(prefix string) bool {
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// newSystem is a System on a temporary home with the programs in path.
func newSystem(t *testing.T, goos string, r *fakeRunner, path map[string]string, env map[string]string) *setup.System {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return &setup.System{
		GOOS: goos, Home: home, ConfigDir: filepath.Join(home, ".config"), CacheDir: filepath.Join(home, ".cache"),
		UID: 1000, Run: r, Out: &bytes.Buffer{}, In: strings.NewReader(""),
		Getenv: func(k string) string { return env[k] },
		LookPath: func(name string) (string, error) {
			if p, ok := path[name]; ok {
				return p, nil
			}
			return "", errNotFound
		},
		Now: func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) },
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
