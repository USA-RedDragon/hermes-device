package web_test

import (
	"os/exec"
	"testing"
)

// chromePath finds Chrome or Chromium for the tests that drive the real
// page, and skips the test where there is none.
func chromePath(t *testing.T) string {
	t.Helper()
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	t.Skip("Chrome is not installed")
	return ""
}
