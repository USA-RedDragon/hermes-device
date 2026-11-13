package hub

import (
	"regexp"
	"strings"
)

// injectedNotes returns the patterns of the notes that Hermes and its plugins append to the end
// of a user message for the model: recalled memory, the subagent route and
// waiting notes of the subagent-routes plugin, and the skills and interface
// notes. Each is matched by its exact opening text and its closing bracket
// or tag at the very end of the message. The page reads the same list in
// attach.js (cutNotes).
func injectedNotes() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?:^|\n)<memory-context>\n\[System note: The following is recalled memory context, [\s\S]*?</memory-context>$`),
		regexp.MustCompile(`(?:^|\n)\[Subagents in this chat \(this replaces any earlier subagent note\): [^\n]*\]$`),
		regexp.MustCompile(`(?:^|\n)\[Subagent routes now: this chat's delegate_task tool was fixed when the chat began and lists [^\n]*\]$`),
		regexp.MustCompile(`(?:^|\n)\[Waiting: to wait for something slow, [^\n]*\]$`),
		regexp.MustCompile(`(?:^|\n)\[System: The skills available in this conversation changed after its system prompt was built\.` +
			`(?: The skills listed in the system prompt are accurate again; earlier skill-change notes are superseded\.|` +
			`\nNow available \(load with skill_view\(name\) when relevant, like the skills listed in the system prompt\):(?:\n    - [^\n]*)+(?:\nNo longer available: [^\n]*)?|` +
			`\nNo longer available: [^\n]*)\]$`),
		regexp.MustCompile(`(?:^|\n)\[System: This conversation is now being answered on a different interface: [^\n]*\]$`),
		regexp.MustCompile(`(?:^|\n)\[System: These tools stay listed for this conversation [^\n]*\]$`),
	}
}

// CutInjectedNotes returns a message without the notes at its end, and
// without the blank lines before them.
func CutInjectedNotes(text string) string {
	notes := injectedNotes()
	for {
		t := strings.TrimRight(text, " \t\r\n")
		var loc []int
		for _, re := range notes {
			if loc = re.FindStringIndex(t); loc != nil {
				break
			}
		}
		if loc == nil {
			return text
		}
		text = strings.TrimRight(t[:loc[0]], " \t\r\n")
	}
}
