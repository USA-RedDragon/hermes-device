package config

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	tokyo  = "Asia/Tokyo"
	berlin = "Europe/Berlin"
)

func TestServerTimeZone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	zone := filepath.Join(dir, "usr", "share", "zoneinfo", filepath.FromSlash(berlin))
	if err := os.MkdirAll(filepath.Dir(zone), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zone, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "localtime")
	if err := os.Symlink(zone, link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	for _, c := range []struct{ tz, localtime, want string }{
		{tokyo, link, tokyo},
		{":Asia/Tokyo", link, tokyo},
		{"", link, berlin},
		{"Not/AZone", link, berlin},
		{"", missing, "UTC"},
		{zone, missing, berlin},
	} {
		if got := serverTimeZone(c.tz, c.localtime); got != c.want {
			t.Errorf("serverTimeZone(%q, %q) = %q, want %q", c.tz, c.localtime, got, c.want)
		}
	}
}
