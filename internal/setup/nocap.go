package setup

import (
	"os"
	"path/filepath"
)

// noCapDropIn is the drop-in that lifted the memory cap of command scopes
// by hand while releases capped them. The cap is gone, so setup removes the
// file, and only that file with exactly this content.
const (
	noCapDropInDir  = "talaria-cmd-.scope.d"
	noCapDropInFile = "50-no-memory-cap.conf"
	noCapDropIn     = "[Scope]\nMemoryMax=infinity\nMemoryHigh=infinity\n"
)

func (s *System) removeNoCapDropIn() {
	if s.GOOS != "linux" {
		return
	}
	dir := filepath.Join(s.Home, ".config", "systemd", "user", noCapDropInDir)
	path := filepath.Join(dir, noCapDropInFile)
	if data, err := os.ReadFile(path); err != nil || string(data) != noCapDropIn {
		return
	}
	if os.Remove(path) == nil {
		_ = os.Remove(dir) // only when empty
	}
}
