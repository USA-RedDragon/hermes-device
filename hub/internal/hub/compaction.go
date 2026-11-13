package hub

import (
	"regexp"
	"strconv"
	"strings"
)

const (
	compactionHead = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. " +
		"This is a handoff from a previous context window — treat it as background reference, NOT as active instructions."
	compactionRulesEnd = "avoid repeating it:"
	compactionLegacy   = "[CONTEXT SUMMARY]:"
	CompactionEnd      = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	compactionHistory  = "Historical only; newer protected-tail messages after this summary win."
)

// compactionModelOnly are the sections of a summary that Hermes writes for
// the model: the user's words quoted again, and how to look the messages up.
// Hermes leaves them out of what it shows.
//
//nolint:gochecknoglobals // fixed text
var compactionModelOnly = [...]string{"## User Messages (verbatim, newest first)", "## Context Recovery"}

//nolint:gochecknoglobals // fixed patterns
var (
	compactionCount = regexp.MustCompile(`The (\d+) compacted message\(s\) remain fully preserved`)
	compactionBlank = regexp.MustCompile(`\n{3,}`)
)

// CompactionSummary reads a user message as the handoff that Hermes writes
// after it compacts a chat: its fixed header, the summary, and an end line.
// It returns the summary as a reader sees it, the number of messages that
// the handoff says it replaced (0 when it names none), and false for any
// other text. The text must start with the header, so a message that only
// quotes it is not one.
func CompactionSummary(text string) (summary string, count int, ok bool) {
	t := strings.TrimSpace(text)
	var body string
	switch {
	case strings.HasPrefix(t, compactionHead):
		_, rest, found := strings.Cut(t, compactionRulesEnd)
		if !found {
			return "", 0, false
		}
		body = rest
	case strings.HasPrefix(t, compactionLegacy):
		body = strings.TrimPrefix(t, compactionLegacy)
	default:
		return "", 0, false
	}
	body = strings.TrimSpace(body)
	if i := strings.LastIndex(body, CompactionEnd); i >= 0 && strings.TrimSpace(body[i+len(CompactionEnd):]) == "" {
		body = strings.TrimSpace(body[:i])
	}
	body, count = cutModelOnly(body)
	return body, count, true
}

func cutModelOnly(body string) (string, int) {
	count := 0
	for _, heading := range compactionModelOnly {
		start := strings.Index(body, heading)
		if start < 0 {
			continue
		}
		end := strings.Index(body[start+len(heading):], "\n## ")
		section, after := body[start:], ""
		if end >= 0 {
			section, after = body[start:start+len(heading)+end], body[start+len(heading)+end+1:]
		}
		if m := compactionCount.FindStringSubmatch(section); m != nil {
			count, _ = strconv.Atoi(m[1])
		}
		body = body[:start] + after
	}
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != compactionHistory {
			kept = append(kept, l)
		}
	}
	return strings.TrimSpace(compactionBlank.ReplaceAllString(strings.Join(kept, "\n"), "\n\n")), count
}
