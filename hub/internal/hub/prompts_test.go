package hub_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

func TestValidatePrompt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		p    hub.SavedPrompt
		want string
	}{
		{hub.SavedPrompt{Name: "Plan-2", Text: "x"}, ""},
		{hub.SavedPrompt{Name: "", Text: "x"}, "Give the prompt a name"},
		{hub.SavedPrompt{Name: "two words", Text: "x"}, "only the letters A to Z, digits and dashes"},
		{hub.SavedPrompt{Name: "café", Text: "x"}, "only the letters A to Z"},
		{hub.SavedPrompt{Name: "a:b", Text: "x"}, "only the letters A to Z"},
		{hub.SavedPrompt{Name: strings.Repeat("a", 65), Text: "x"}, "at most 64 characters"},
		{hub.SavedPrompt{Name: "ok", Text: " \n "}, "Give the prompt some text"},
		{hub.SavedPrompt{Name: "ok", Text: strings.Repeat("x", 64<<10+1)}, "longer than 64 KB"},
	} {
		err := hub.ValidatePrompt(c.p)
		if c.want == "" {
			if err != nil {
				t.Errorf("%q: %v", c.p.Name, err)
			}
			continue
		}
		if !errors.Is(err, hub.ErrBadPrompt) || !strings.Contains(hub.PromptReason(err), c.want) {
			t.Errorf("%q: %v, want %q", c.p.Name, err, c.want)
		}
	}
}
