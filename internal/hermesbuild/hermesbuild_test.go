//go:build unix

package hermesbuild_test

import (
	"archive/tar"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/hermesbuild"
	"github.com/USA-RedDragon/talaria/internal/oci"
	"github.com/USA-RedDragon/talaria/internal/oci/ocitest"
)

var imageEnv = []string{ //nolint:gochecknoglobals // test fixture
	"PATH=/opt/hermes/bin:/usr/bin", "HERMES_HOME=/opt/data", "HERMES_WRITE_SAFE_ROOT=/opt/data",
	"HERMES_TUI_DIR=/opt/hermes/ui-tui", "HERMES_PYTHON=/opt/hermes/.venv/bin/python", "HERMES_GONE=/opt/hermes/missing",
	"HERMES_ESCAPE=/../../../../../../tmp",
	"PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1", "PLAYWRIGHT_BROWSERS_PATH=/opt/hermes/tools", "CC=gcc",
}

// hermesLayers is a small image laid out like the Hermes image.
func hermesLayers(t *testing.T, version string) [][]byte {
	t.Helper()
	launcher := "#!/bin/sh\necho \"Hermes Agent v" + version + " (2026.9.24)\"\necho \"HOME=$HOME\"\n"
	return [][]byte{
		ocitest.Layer(t,
			ocitest.Entry{Name: "usr/bin/apt", Body: "apt"},
			ocitest.Entry{Name: "opt/hermes/tools/py/bin/python3", Body: "#!/bin/sh\n", Mode: 0o755},
			ocitest.Entry{Name: "opt/hermes/tools/node/bin/node", Body: "#!/bin/sh\n", Mode: 0o755},
			ocitest.Entry{Name: "opt/hermes/tools/chromium-1208/chrome", Body: "browser"},
			ocitest.Entry{Name: "usr/local/bin/python3", Type: tar.TypeSymlink, Link: "/opt/hermes/tools/py/bin/python3"},
			ocitest.Entry{Name: "usr/local/bin/node", Type: tar.TypeSymlink, Link: "/opt/hermes/tools/node/bin/node"},
		),
		ocitest.Layer(t,
			ocitest.Entry{Name: "opt/hermes/.venv/pyvenv.cfg", Body: "home = /usr/local/bin\nimplementation = CPython\nrelocatable = true\n"},
			ocitest.Entry{Name: "opt/hermes/.venv/bin/python", Type: tar.TypeSymlink, Link: "/usr/local/bin/python3"},
			ocitest.Entry{Name: "opt/hermes/libexec/hermes", Body: launcher, Mode: 0o755},
			ocitest.Entry{Name: "opt/hermes/.venv/bin/hermes", Type: tar.TypeSymlink, Link: "../../libexec/hermes"},
			ocitest.Entry{Name: "opt/hermes/ui-tui/dist/entry.js", Body: "//"},
		),
	}
}

func newStore(t *testing.T, r *ocitest.Registry) *hermesbuild.Store {
	t.Helper()
	return &hermesbuild.Store{Dir: filepath.Join(t.TempDir(), "hermes"), HTTP: r.Client(), Arch: "amd64",
		Log: slog.New(slog.DiscardHandler)}
}

func push(t *testing.T, r *ocitest.Registry, version string) string {
	t.Helper()
	img := r.Push(t, oci.MediaOCILayerGzip, imageEnv, hermesLayers(t, version)...)
	return r.Reference(img.Index)
}

func TestEnsureRelocatesAndRuns(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	image := push(t, r, "0.23.1")
	s := newStore(t, r)
	b, err := s.Ensure(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	if b.Meta.Reference != image || b.Meta.Source != image || !slices.Equal(b.Meta.Env, imageEnv) {
		t.Errorf("meta = %+v", b.Meta)
	}
	link, err := os.Readlink(filepath.Join(b.Dir, "opt/hermes/.venv/bin/python"))
	if err != nil || link != "../../../../usr/local/bin/python3" {
		t.Errorf("python link = %q, %v", link, err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(b.Dir, "opt/hermes/.venv/bin/python"))
	// b.Dir may itself be under a symlink (on macOS, /var is /private/var)
	want, _ := filepath.EvalSymlinks(filepath.Join(b.Dir, "opt/hermes/tools/py/bin/python3"))
	if err != nil || resolved != want {
		t.Errorf("python resolves to %q, %v", resolved, err)
	}
	cfg, err := os.ReadFile(filepath.Join(b.Dir, "opt/hermes/.venv/pyvenv.cfg"))
	if err != nil || !strings.HasPrefix(string(cfg), "home = "+filepath.Join(b.Dir, "usr/local/bin")+"\nimplementation = CPython\n") {
		t.Errorf("pyvenv.cfg = %q, %v", cfg, err)
	}
	for _, gone := range []string{"usr/bin", "opt/hermes/tools/chromium-1208"} {
		if _, err := os.Lstat(filepath.Join(b.Dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was extracted", gone)
		}
	}
	launcher, err := b.Launcher()
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.Env(b, []string{"HOME=/home/alice", "PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), launcher, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "Hermes Agent v0.23.1 (2026.9.24)\nHOME=" + s.Home() + "\n"; string(out) != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	again, err := s.Ensure(context.Background(), image)
	if err != nil || again.Dir != b.Dir {
		t.Errorf("second Ensure = %+v, %v", again, err)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			m[k] = "DUPLICATE"
			continue
		}
		m[k] = v
	}
	return m
}

func TestEnvIsolatesTheHome(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	s := newStore(t, r)
	b, err := s.Ensure(context.Background(), push(t, r, "0.23.1"))
	if err != nil {
		t.Fatal(err)
	}
	userHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(userHome, ".Xauthority"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := s.Env(b, []string{
		"HOME=" + userHome, "PATH=" + userHome + "/.nvm/versions/node/v24/bin:/usr/bin:relative:" + userHome, "TERM=xterm-256color",
		"LD_PRELOAD=/x.so", "LD_LIBRARY_PATH=/home/alice/lib", "NVM_DIR=/home/alice/.nvm", "COREPACK_HOME=/c", "LANG=C.UTF-8", "HERMES_HOME=" + userHome + "/.hermes",
		"HERMES_NODE=/home/alice/.nvm/node", "PYTHONPATH=/x", "PYTHONHOME=/y", "VIRTUAL_ENV=/v", "NODE_OPTIONS=--x",
		"npm_config_prefix=/home/alice/.local", "UV_CACHE_DIR=/c", "XDG_CONFIG_HOME=/home/alice/.config",
		"XDG_RUNTIME_DIR=/run/user/1000", "WAYLAND_DISPLAY=wayland-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	home := s.Home()
	bin := filepath.Join(b.Dir, "usr/local/bin")
	want := map[string]string{
		"HOME": home, "HERMES_HOME": s.HermesHome(), "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_CACHE_HOME": filepath.Join(home, ".cache"), "XDG_DATA_HOME": filepath.Join(home, ".local/share"),
		"XDG_STATE_HOME": filepath.Join(home, ".local/state"), "HERMES_MANAGED_DIR": filepath.Join(home, "managed-scope"),
		"PATH":           bin + ":" + filepath.Join(b.Dir, "opt/hermes/.venv/bin") + ":/usr/bin",
		"HERMES_NODE":    filepath.Join(bin, "node"),
		"HERMES_TUI_DIR": filepath.Join(b.Dir, "opt/hermes/ui-tui"), "HERMES_PYTHON": filepath.Join(b.Dir, "opt/hermes/.venv/bin/python"),
		"PLAYWRIGHT_BROWSERS_PATH": filepath.Join(b.Dir, "opt/hermes/tools"), "PYTHONUNBUFFERED": "1",
		"TERM": "xterm-256color", "LANG": "C.UTF-8", "XDG_RUNTIME_DIR": "/run/user/1000", "WAYLAND_DISPLAY": "wayland-0",
		"XAUTHORITY": filepath.Join(userHome, ".Xauthority"),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"PYTHONPATH", "PYTHONHOME", "VIRTUAL_ENV", "NODE_OPTIONS", "npm_config_prefix", "UV_CACHE_DIR",
		"HERMES_WRITE_SAFE_ROOT", "PYTHONDONTWRITEBYTECODE", "HERMES_GONE", "HERMES_ESCAPE", "CC", "LD_PRELOAD", "LD_LIBRARY_PATH",
		"NVM_DIR", "COREPACK_HOME"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q is set", k, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("env has %d variables, want %d: %v", len(got), len(want), env)
	}
	if fi, err := os.Stat(s.HermesHome()); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("HERMES_HOME: %v, %v", fi, err)
	}
}

func TestEnsurePullsOnce(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	img := r.Push(t, oci.MediaOCILayerGzip, imageEnv, hermesLayers(t, "0.23.1")...)
	image := r.Reference(img.Index)
	s := newStore(t, r)
	var wg sync.WaitGroup
	dirs := make([]string, 4)
	for i := range dirs {
		wg.Go(func() {
			// Separate stores share the directory, as the daemon and a
			// chat do.
			other := *s
			b, err := other.Ensure(context.Background(), image)
			if err != nil {
				t.Error(err)
				return
			}
			dirs[i] = b.Dir
		})
	}
	wg.Wait()
	if n := r.Requests("/v2/" + r.Repo + "/blobs/" + img.Layers[0]); n != 1 {
		t.Errorf("layer fetched %d times, want 1", n)
	}
	for _, d := range dirs {
		if d != dirs[0] {
			t.Errorf("dirs = %v", dirs)
		}
	}
}

func TestEnsureRefuses(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	s := newStore(t, r)
	if _, err := s.Ensure(context.Background(), r.Domain()+"/"+r.Repo+":v1"); !errors.Is(err, oci.ErrNoDigest) {
		t.Errorf("no digest: %v", err)
	}
	img := r.Push(t, oci.MediaOCILayerGzip, imageEnv, hermesLayers(t, "0.23.1")...)
	r.Tamper(img.Layers[1], ocitest.Layer(t, ocitest.Entry{Name: "opt/hermes/libexec/hermes", Body: "#!/bin/sh\necho evil\n", Mode: 0o755}))
	image := r.Reference(img.Index)
	if _, err := s.Ensure(context.Background(), image); !errors.Is(err, oci.ErrDigest) {
		t.Fatalf("tampered layer: %v", err)
	}
	if _, err := s.Open(image); !errors.Is(err, hermesbuild.ErrNotPulled) {
		t.Errorf("Open after a failed pull: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(s.Dir, "staging")); len(ents) != 0 {
		t.Errorf("staging holds %d entries", len(ents))
	}
}

func TestPruneKeepsCurrentPreviousAndInUse(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	s := newStore(t, r)
	ctx := context.Background()
	images := make([]string, 0, 4)
	for i, v := range []string{"0.21.0", "0.22.0", "0.23.0", "0.23.1"} {
		image := push(t, r, v)
		b, err := s.Ensure(ctx, image)
		if err != nil {
			t.Fatal(err)
		}
		used := time.Now().Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(filepath.Join(b.Dir, ".build.json"), used, used); err != nil {
			t.Fatal(err)
		}
		images = append(images, image)
	}
	present := func() []string {
		var out []string
		for _, image := range images {
			if _, err := s.Open(image); err == nil {
				out = append(out, image)
			}
		}
		return out
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, "staging", strings.Repeat("a", 64)+"-123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(images[3], 2); err != nil {
		t.Fatal(err)
	}
	if got := present(); !slices.Equal(got, images[1:]) {
		t.Errorf("after keeping 2: %v", got)
	}
	if ents, _ := os.ReadDir(filepath.Join(s.Dir, "staging")); len(ents) != 0 {
		t.Errorf("staging holds %d entries", len(ents))
	}
	oldest, err := s.Open(images[1])
	if err != nil {
		t.Fatal(err)
	}
	inUse, err := s.Use(ctx, oldest)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(images[1]); !errors.Is(err, hermesbuild.ErrInUse) {
		t.Errorf("Remove of a build in use: %v", err)
	}
	if err := s.Prune(images[3], 0); err != nil {
		t.Fatal(err)
	}
	if got := present(); !slices.Equal(got, []string{images[1], images[3]}) {
		t.Errorf("after a prune with a build in use: %v", got)
	}
	_ = inUse.Close()
	if err := s.Prune(images[3], 0); err != nil {
		t.Fatal(err)
	}
	if got := present(); !slices.Equal(got, []string{images[3]}) {
		t.Errorf("after the last prune: %v", got)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Errorf("List = %v, %v", list, err)
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	img := r.Push(t, oci.MediaOCILayerGzip, imageEnv, hermesLayers(t, "0.23.1")...)
	image := r.Reference(img.Index)
	s := newStore(t, r)
	ctx := context.Background()
	b, err := s.Ensure(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(b.Dir, "opt/hermes/libexec/hermes")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(launcher, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, image); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(launcher); !strings.Contains(string(data), "Hermes Agent v0.23.1") {
		t.Errorf("launcher after refresh = %q", data)
	}
	r.Tamper(img.Layers[0], []byte("not the layer"))
	if _, err := s.Refresh(ctx, image); !errors.Is(err, oci.ErrDigest) {
		t.Errorf("refresh from a bad registry: %v", err)
	}
	if _, err := s.Open(image); err != nil {
		t.Errorf("the build is gone after a failed refresh: %v", err)
	}
	b, err = s.Open(image)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.Use(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer keep.Close()
	if !s.InUse(b) {
		t.Error("InUse is false while a chat uses the build")
	}
	if _, err := s.Refresh(ctx, image); !errors.Is(err, hermesbuild.ErrInUse) {
		t.Errorf("refresh in use: %v", err)
	}
}
