package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/config"
)

func TestUsageSwitch(t *testing.T) {
	off, on := false, true
	state := t.TempDir()
	if deviceUsage(&config.Config{Usage: config.Usage{Enabled: &off}}, state) != nil {
		t.Fatal("run counts usage with usage.enabled false")
	}
	if deviceUsage(&config.Config{Usage: config.Usage{Enabled: &on}}, state) == nil {
		t.Fatal("run does not count usage with usage.enabled true")
	}
	if hermesUsage(&config.Config{Usage: config.Usage{Enabled: &off}}) != nil {
		t.Fatal("acp-host counts usage with usage.enabled false")
	}
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	t.Setenv("CLAUDE_ACCOUNTS_ROOT", "")
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(
		"plugins:\n  entries:\n    claude-accounts:\n      settings:\n        accounts:\n          - {id: work, label: Work}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := hermesUsage(&config.Config{})
	if c == nil {
		t.Fatal("acp-host does not count usage by default")
	}
	rep := c.Report()
	if len(rep.Accounts) != 1 || rep.Accounts[0].Slot != "work" || rep.Accounts[0].Label != "Work" {
		t.Fatalf("accounts %+v", rep.Accounts)
	}
}
