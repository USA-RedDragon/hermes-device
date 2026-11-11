package hermesbuild

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// launchers are the image paths of the Hermes launcher, tried in order:
// the venv entry point, which the image's own docker-exec shim runs.
var launchers = []string{Prefix + "/.venv/bin/hermes", Prefix + "/libexec/hermes"} //nolint:gochecknoglobals // fixed table

// Launcher is the path of b's hermes launcher.
func (b *Build) Launcher() (string, error) {
	for _, l := range launchers {
		p := filepath.Join(b.Dir, l)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o100 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: none of %v in %s", ErrNoLauncher, launchers, b.Dir)
}

// Home is the HOME of the managed Hermes, and HermesHome its
// HERMES_HOME.
func (s *Store) Home() string       { return filepath.Join(s.Dir, homeDir) }
func (s *Store) HermesHome() string { return filepath.Join(s.Home(), ".hermes") }

const (
	envHome = "HOME"
	envPath = "PATH"
)

// dropEnv lists the variables of the user's environment that the managed
// Hermes must not see: they point Python, Node or Hermes at shared
// installs or at the user's own Hermes home.
var dropEnv = []string{ //nolint:gochecknoglobals // fixed table
	envHome, envPath, "VIRTUAL_ENV", "CONDA_PREFIX", "CONDA_DEFAULT_ENV", "NODE_OPTIONS", "NODE_PATH",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
}

var dropEnvPrefixes = []string{ //nolint:gochecknoglobals // fixed table
	"HERMES_", "PYTHON", "UV_", "PLAYWRIGHT_", "NPM_CONFIG_", "PNPM_", "PIP_", "NVM_", "COREPACK_", "LD_PRELOAD", "LD_LIBRARY_PATH",
}

// imageEnvPrefixes select the image variables the TUI gets; a value that
// is an absolute path moves under the build.
var imageEnvPrefixes = []string{"HERMES_", "PYTHON", "PLAYWRIGHT_"} //nolint:gochecknoglobals // fixed table

// skipImageEnv are image variables the managed Hermes must not get: its
// home is the store's, and it may cache bytecode there, which starts it
// about five times faster.
var skipImageEnv = []string{"HERMES_HOME", "HERMES_WRITE_SAFE_ROOT", "PYTHONDONTWRITEBYTECODE"} //nolint:gochecknoglobals // fixed table

func dropped(name string) bool {
	return slices.Contains(dropEnv, name) || hasPrefix(strings.ToUpper(name), dropEnvPrefixes)
}

// Env is the environment of b's TUI: environ without the variables that
// lead to shared installs or the user's Hermes, the image's Hermes and
// Python variables moved under the build, and HOME, HERMES_HOME and the
// XDG base directories in the store's home. It creates the home.
func (s *Store) Env(b *Build, environ []string) ([]string, error) {
	home := s.Home()
	managedScope := filepath.Join(home, "managed-scope")
	dirs := map[string]string{
		envHome: home, "HERMES_HOME": s.HermesHome(),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"XDG_DATA_HOME": filepath.Join(home, ".local", "share"), "XDG_STATE_HOME": filepath.Join(home, ".local", "state"),
		"HERMES_MANAGED_DIR": managedScope,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	var userHome, userPath string
	out := make([]string, 0, len(environ)+len(dirs)+8)
	hasXauth := false
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case envHome:
			userHome = v
		case envPath:
			userPath = v
		case "XAUTHORITY":
			hasXauth = true
		}
		if !dropped(k) {
			out = append(out, kv)
		}
	}
	for _, kv := range b.Meta.Env {
		k, v, _ := strings.Cut(kv, "=")
		if slices.Contains(skipImageEnv, k) || !hasPrefix(k, imageEnvPrefixes) {
			continue
		}
		if filepath.IsAbs(v) {
			p := filepath.Join(b.Dir, v)
			if !within(b.Dir, p) {
				continue
			}
			if _, err := os.Lstat(p); err != nil {
				continue
			}
			v = p
		}
		out = append(out, k+"="+v)
	}
	for k, v := range dirs {
		out = append(out, k+"="+v)
	}
	bin := filepath.Join(b.Dir, "usr", "local", "bin")
	if node := filepath.Join(bin, "node"); executable(node) {
		out = append(out, "HERMES_NODE="+node)
	}
	path := []string{bin, filepath.Join(b.Dir, Prefix, ".venv", "bin")}
	for _, d := range filepath.SplitList(userPath) {
		// Directories in the user's home hold the user's own installs,
		// such as nvm, ~/.local/bin or a virtualenv.
		if filepath.IsAbs(d) && (userHome == "" || !within(userHome, d)) {
			path = append(path, d)
		}
	}
	out = append(out, envPath+"="+strings.Join(path, string(os.PathListSeparator)))
	if !hasXauth && userHome != "" {
		if xa := filepath.Join(userHome, ".Xauthority"); exists(xa) {
			out = append(out, "XAUTHORITY="+xa)
		}
	}
	return out, nil
}

// within reports whether p is dir or inside it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o100 != 0
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, fs.ErrNotExist)
}
