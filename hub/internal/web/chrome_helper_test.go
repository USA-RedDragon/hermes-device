package web_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// chromeSlots caps how many tests run a browser (Chrome or Firefox) at once: a
// dozen cold starts on a small runner outlast the 30 s wait for the DevTools
// address, and Firefox runs left out of the cap starved the Chrome ones.
const chromeSlots = 2

// chromePath finds Chrome or Chromium for the tests that drive the real
// page, and skips the test where there is none. It also takes one of the
// chromeSlots, held until the test ends.
func chromePath(t *testing.T) string {
	t.Helper()
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			takeChromeSlot(t)
			return p
		}
	}
	t.Skip("Chrome is not installed")
	return ""
}

// takeChromeSlot blocks until one of the lock files is free. A file lock
// needs no package-level state and also bounds the other packages' tests.
func takeChromeSlot(t *testing.T) {
	t.Helper()
	for {
		for i := range chromeSlots {
			f, err := os.OpenFile(filepath.Join(os.TempDir(), fmt.Sprintf("talaria-hub-chrome-%d.lock", i)), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				t.Cleanup(func() { _ = f.Close() })
				return
			}
			_ = f.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
}
