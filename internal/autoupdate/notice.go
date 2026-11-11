package autoupdate

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Notice limits for interactive commands.
const (
	NoticeMaxAge  = 24 * time.Hour
	NoticeTimeout = 5 * time.Second
	// NoticeGrace is how long a command waits at its end for a lookup it
	// started, so that a short command still fills the cache.
	NoticeGrace = 2 * time.Second
)

// Notice tells a person who runs a command that a newer release exists.
type Notice struct {
	Releases *Releases
	// Version is the running binary's version.
	Version string
	// Auto is update.auto: the daemon installs new releases by itself.
	Auto bool
	Out  io.Writer

	done    chan struct{}
	printed bool
}

// Start prints the notice when the cache already knows a newer release,
// and refreshes the cache in the background when no lookup was tried in
// the last NoticeMaxAge. It never waits for the network.
func (n *Notice) Start(ctx context.Context) {
	if !Stable(vPrefix(n.Version)) {
		return
	}
	if tag, _ := n.Releases.Cached(); Newer(tag, n.Version) {
		n.print(tag)
	}
	n.done = make(chan struct{})
	go func() {
		defer close(n.done)
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), NoticeTimeout)
		defer cancel()
		_, _ = n.Releases.Latest(lctx, NoticeMaxAge)
	}()
}

// Finish waits up to NoticeGrace for the background lookup, and prints the
// notice when it found a newer release and Start printed none.
func (n *Notice) Finish() {
	if n.done == nil {
		return
	}
	select {
	case <-n.done:
	case <-time.After(NoticeGrace):
		return
	}
	if tag, _ := n.Releases.Cached(); !n.printed && Newer(tag, n.Version) {
		n.print(tag)
	}
}

func (n *Notice) print(tag string) {
	n.printed = true
	how := "Run talaria update to install it."
	if n.Auto {
		how = "The talaria service installs it by itself (update.auto is on), or run talaria update."
	}
	_, _ = fmt.Fprintf(n.Out, "talaria %s is available (this is %s). %s\n", tag, vPrefix(n.Version), how)
}

func vPrefix(v string) string {
	if len(v) > 0 && v[0] != 'v' {
		return "v" + v
	}
	return v
}
