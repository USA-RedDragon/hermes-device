package hub

import "strings"

const (
	compactionHead = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. " +
		"This is a handoff from a previous context window — treat it as background reference, NOT as active instructions."
	compactionRulesEnd = "avoid repeating it:"
	compactionLegacy   = "[CONTEXT SUMMARY]:"
	compactionEnd      = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
)

// CompactionSummary reads a user message as the handoff that Hermes writes
// after it compacts a chat: its fixed header, the summary, and an end line.
// It returns the summary alone, and false for any other text. The text must
// start with the header, so a message that only quotes it is not one.
func CompactionSummary(text string) (string, bool) {
	t := strings.TrimSpace(text)
	var body string
	switch {
	case strings.HasPrefix(t, compactionHead):
		_, rest, ok := strings.Cut(t, compactionRulesEnd)
		if !ok {
			return "", false
		}
		body = rest
	case strings.HasPrefix(t, compactionLegacy):
		body = strings.TrimPrefix(t, compactionLegacy)
	default:
		return "", false
	}
	body = strings.TrimSpace(body)
	if i := strings.LastIndex(body, compactionEnd); i >= 0 && strings.TrimSpace(body[i+len(compactionEnd):]) == "" {
		body = strings.TrimSpace(body[:i])
	}
	return body, true
}
