package telegram

import "time"

// WithStatusTick makes the status line of a running turn show the time
// again every d.
func WithStatusTick(d time.Duration) func(*Options) {
	return func(o *Options) { o.statusTick = d }
}

// WithEditGaps sets the least time between two edits in a private chat
// and in a group.
func WithEditGaps(private, group time.Duration) func(*Options) {
	return func(o *Options) { o.editGaps = [2]time.Duration{private, group} }
}

// Plain drops the emoji Hermes puts in its labels and progress.
func Plain(s string) string { return plain(s) }

// PromptChunks is how many messages a saved prompt's text takes.
func PromptChunks(text string) int { return len(promptChunks(text)) }
