package cmd

import (
	"testing"
)

const browserName = "browser"

func TestMigrationCommands(t *testing.T) {
	t.Parallel()
	root := NewCommand("0.19.0", "test")
	for _, tc := range []struct {
		args []string
		skip bool
	}{
		{[]string{"run"}, false},
		{[]string{"run", "--" + CheckConfigFlag}, true},
		{[]string{"acp-host"}, true},
		{[]string{"plugin", "install"}, true},
		{[]string{browserName, "install"}, false},
		{[]string{browserName, "extension"}, true},
		{[]string{"chat"}, false},
		{[]string{"acp"}, false},
		{[]string{"setup"}, false},
		{[]string{"update"}, false},
	} {
		c, rest, err := root.Find(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if err := c.ParseFlags(rest); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got := skipMigration(c); got != tc.skip {
			t.Errorf("%v: skipMigration = %t", tc.args, got)
		}
	}
}
