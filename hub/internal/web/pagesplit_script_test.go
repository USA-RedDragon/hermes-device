package web_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A reply that reaches the page in pieces cut inside a fenced block stays
// one bubble, so its page block is one card: a stored row, an interim and a
// message handed to the running turn each start a new bubble only when the
// text so far has no open fence.
func TestPiecesInsideAFenceStayOneBubble(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for name, re := range map[string]string{
		"a stored assistant row": `(?s)if \(text\) \{\s*this\.assistantText\(text\);.*?if \(!window\.HubPageBlocks\.open\(this\.turn\.raw\)\) this\.turn\.text = null;`,
		"message.interim":        `(?s)case "message\.interim":.*?if \(!window\.HubPageBlocks\.open\(this\.turn\.raw\)\) this\.turn\.text = null;.*?break;`,
		"message.start":          `case "message\.start":[^}]*?HubPageBlocks\.open\(this\.turn\.raw\)\)\) this\.turn = null;`,
	} {
		if !regexp.MustCompile(re).MatchString(src) {
			t.Errorf("%s starts a new bubble without asking HubPageBlocks.open", name)
		}
	}
	if strings.Contains(src, `this.assistantText(p.text); this.turn.text = null; }`) {
		t.Error("message.interim still always ends the bubble")
	}
}
