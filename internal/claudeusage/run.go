package claudeusage

import (
	"context"
	"runtime/debug"
	"slices"
	"time"
)

// Timing of Run.
const (
	ScanInterval = 30 * time.Second
	saveInterval = 5 * time.Minute
	fullInterval = 24 * time.Hour
	maxBackoff   = 30 * time.Minute
)

// batchRows is about the most rows per request; an hour is never split.
const batchRows = 2000

// Sender delivers a report to the hub.
type Sender func(ctx context.Context, r Report) error

// Run scans every ScanInterval until ctx ends and keeps the state file.
// When send is not nil it hands it every hour whose rows changed since
// the last delivery, and every hour from the report's Since once a day
// and after a start.
func (c *Collector) Run(ctx context.Context, send Sender) {
	c.Load()
	var sent map[int64][]Row
	var sentAccounts []Account
	var lastSave, lastFull, nextTry time.Time
	backoff := ScanInterval
	first := true
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := c.Save(); err != nil {
				c.log.Warn("Could not save the Claude usage state", "error", err)
			}
			return
		case <-t.C:
		}
		if _, err := c.Scan(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("Could not read the Claude Code transcripts", "error", err)
		}
		if first {
			debug.FreeOSMemory()
			first = false
		}
		if time.Since(lastSave) >= saveInterval {
			if err := c.Save(); err != nil {
				c.log.Warn("Could not save the Claude usage state", "error", err)
			}
			lastSave = time.Now()
		}
		if send != nil && !time.Now().Before(nextTry) {
			if time.Since(lastFull) >= fullInterval {
				sent = nil
			}
			rep := c.Report()
			cur := byHour(rep.Rows)
			hours := changedHours(sent, cur, rep.Since, c.o.Now())
			if len(hours) > 0 || sent == nil || !slices.Equal(sentAccounts, rep.Accounts) {
				if err := c.deliver(ctx, send, hours, cur, rep.Accounts); err != nil {
					if ctx.Err() == nil {
						c.log.Warn("Could not send Claude usage to the hub", "error", err)
					}
					nextTry = time.Now().Add(backoff)
					backoff = min(backoff*2, maxBackoff)
				} else {
					backoff = ScanInterval
					if sent == nil {
						lastFull = time.Now()
					}
					sent, sentAccounts = cur, rep.Accounts
				}
			}
		}
		t.Reset(ScanInterval)
	}
}

func byHour(rows []Row) map[int64][]Row {
	out := map[int64][]Row{}
	for _, r := range rows {
		out[r.Hour] = append(out[r.Hour], r)
	}
	return out
}

// changedHours are the hours to send: every hour from since to now when
// nothing was sent yet, else the hours whose rows differ from sent.
func changedHours(sent, cur map[int64][]Row, since int64, now time.Time) []int64 {
	var out []int64
	if sent == nil {
		for h := since; h <= now.Unix(); h += 3600 {
			out = append(out, h)
		}
		for h := range cur {
			if h > now.Unix() {
				out = append(out, h)
			}
		}
		slices.Sort(out)
		return out
	}
	for h, rows := range cur {
		if !slices.Equal(sent[h], rows) {
			out = append(out, h)
		}
	}
	for h := range sent {
		if _, ok := cur[h]; !ok && h >= since {
			out = append(out, h)
		}
	}
	slices.Sort(out)
	return out
}

// deliver sends the rows of hours in batches of about batchRows rows.
// With no hours it still sends the accounts.
func (c *Collector) deliver(ctx context.Context, send Sender, hours []int64, cur map[int64][]Row, accounts []Account) error {
	if len(hours) == 0 {
		return send(ctx, Report{Rows: []Row{}, Accounts: accounts})
	}
	b := Report{Rows: []Row{}, Accounts: accounts}
	for i, h := range hours {
		b.Hours = append(b.Hours, h)
		b.Rows = append(b.Rows, cur[h]...)
		if len(b.Rows) < batchRows && i < len(hours)-1 {
			continue
		}
		if err := send(ctx, b); err != nil {
			return err
		}
		b = Report{Rows: []Row{}, Accounts: accounts}
	}
	return nil
}
