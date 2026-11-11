package telegram

import "time"

// WithStatusTick makes the status line of a running turn show the time
// again every d.
func WithStatusTick(d time.Duration) func(*Options) {
	return func(o *Options) { o.statusTick = d }
}
