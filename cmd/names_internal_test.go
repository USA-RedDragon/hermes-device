package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestHelpUsesProductNames(t *testing.T) {
	t.Parallel()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, text := range []string{c.Short, c.Long} {
			for _, old := range []string{"hermes-hub", "Hermes Firefox", "Hermes extension"} {
				if strings.Contains(text, old) {
					t.Errorf("%q: help text names %q: %s", c.CommandPath(), old, text)
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewCommand("v0.0.0", "none"))
}
