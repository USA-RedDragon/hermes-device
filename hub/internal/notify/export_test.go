package notify

import (
	"context"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func (n *Notifier) HandleNow(ctx context.Context, nt hub.Notice) { n.handle(ctx, nt) }
func (n *Notifier) RemindNow(ctx context.Context)                { n.remind(ctx) }
func (n *Notifier) PollJobsNow(ctx context.Context)              { n.pollJobs(ctx) }

func Quiet(s hub.NotifySettings, t time.Time) bool { return quiet(s, t) }
