package plugininstall_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/USA-RedDragon/talaria/internal/plugininstall"
	"github.com/USA-RedDragon/talaria/plugin"
)

func TestInstallReplacesTree(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "plugins", "device")
	v1 := fstest.MapFS{"plugin.yaml": {Data: []byte("name: device\n")}, "old.py": {Data: []byte("x")}}
	if err := plugininstall.Install(v1, dest, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := plugininstall.Install(plugin.Files(), dest, "v2\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "old.py")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old.py left behind: %v", err)
	}
	for _, f := range []string{"plugin.yaml", "__init__.py", "core.py", "dashboard/plugin_api.py", "dashboard/manifest.json"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "tests")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tests installed: %v", err)
	}
	v, err := os.ReadFile(filepath.Join(dest, plugininstall.VersionFile))
	if err != nil || string(v) != "v2\n" {
		t.Errorf("VERSION = %q, %v", v, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("staging left behind: %v", entries)
	}
}

func TestInstallRefusesForeignDir(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plugininstall.Install(plugin.Files(), dest, "v"); !errors.Is(err, plugininstall.ErrNotPluginDir) {
		t.Errorf("err = %v", err)
	}
	if err := plugininstall.Install(plugin.Files(), "relative/dir", "v"); err == nil {
		t.Error("relative destination accepted")
	}
}
