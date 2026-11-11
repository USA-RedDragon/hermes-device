package legacy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/legacy"
)

func TestDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir := filepath.Join(base, "talaria")
	if got := legacy.Dir(dir); got != dir {
		t.Errorf("no directories: %s", got)
	}
	old := filepath.Join(base, "hermes-device")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := legacy.Dir(dir); got != old {
		t.Errorf("only the old directory: %s", got)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := legacy.Dir(dir); got != dir {
		t.Errorf("both directories: %s", got)
	}
	if got := legacy.Dir("/x/other"); got != "/x/other" {
		t.Errorf("other name: %s", got)
	}
}
