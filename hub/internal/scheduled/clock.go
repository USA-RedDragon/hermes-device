package scheduled

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// whenLayout is how the page shows a time, such as "Tue Nov 17, 1:00 AM".
const whenLayout = "Mon Jan 2, 3:04 PM"

// Kinds of a Hermes schedule (cron/jobs.py parse_schedule).
const (
	kindCron     = "cron"
	kindOnce     = "once"
	kindInterval = "interval"
)

// zoneName is a friendly name of a common zone for the page's heading.
func zoneName(zone string) (string, bool) {
	switch zone {
	case "America/Chicago":
		return "Central", true
	case "America/New_York":
		return "Eastern", true
	case "America/Denver", "America/Phoenix":
		return "Mountain", true
	case "America/Los_Angeles":
		return "Pacific", true
	case "America/Anchorage":
		return "Alaska", true
	case "Pacific/Honolulu":
		return "Hawaii", true
	case "UTC":
		return "UTC", true
	}
	return "", false
}

// ZoneLabel names loc for the page, such as "Central (CST)", with the
// abbreviation in force at now.
func ZoneLabel(loc *time.Location, now time.Time) string {
	abbr := now.In(loc).Format("MST")
	name, ok := zoneName(loc.String())
	if !ok {
		name = loc.String()
	}
	if name == abbr {
		return name
	}
	return name + " (" + abbr + ")"
}

// When formats t in loc, such as "Tue Nov 17, 1:00 AM".
func When(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(whenLayout)
}

// Relative says how far t is from now, such as "in 25 min", "in 6 h" or
// "in 5 days". Days count calendar days in loc.
func Relative(now, t time.Time, loc *time.Location) string {
	d := t.Sub(now)
	switch {
	case d < time.Minute:
		if d < -time.Minute {
			return "overdue"
		}
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %d min", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %d h", int(d/time.Hour))
	}
	a, b := now.In(loc), t.In(loc)
	days := int(math.Round(time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC).
		Sub(time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24))
	if days <= 1 {
		return "tomorrow"
	}
	return fmt.Sprintf("in %d days", days)
}

// Duration formats a run's length, such as "31s", "2m 41s" or "1h 04m".
func Duration(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 0:
		return ""
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
}

// ParseTime reads a time Hermes wrote with Python's isoformat. A time
// without an offset is wall time in loc.
func ParseTime(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// epoch reads a Unix time in seconds, as Hermes's session rows carry.
func epoch(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9))
}

// nextRuns are the next n times of a schedule as Hermes parsed it
// (cron/jobs.py parse_schedule), as cron/jobs.py compute_next_run counts
// them for a job that has not run: a cron expression on the wall clock of
// loc, an interval from now, a one-shot at its time. It never reads the
// schedule's text; ok is false for a schedule it cannot follow, such as a
// six-field cron expression.
func nextRuns(s schedule, now time.Time, loc *time.Location, n int) ([]time.Time, bool) {
	var out []time.Time
	switch s.Kind {
	case kindOnce:
		t, ok := ParseTime(string(s.RunAt), loc)
		if !ok {
			return nil, false
		}
		return []time.Time{t}, true
	case kindInterval:
		if s.Minutes <= 0 {
			return nil, false
		}
		step := time.Duration(float64(s.Minutes) * float64(time.Minute))
		t := now
		for range n {
			t = t.Add(step)
			out = append(out, t)
		}
		return out, true
	case kindCron:
		if len(strings.Fields(string(s.Expr))) != 5 {
			return nil, false
		}
		sched, err := cron.ParseStandard(string(s.Expr))
		if err != nil {
			return nil, false
		}
		t := now.In(loc)
		for range n {
			t = sched.Next(t)
			if t.IsZero() {
				break
			}
			out = append(out, t)
		}
		return out, len(out) > 0
	}
	return nil, false
}
