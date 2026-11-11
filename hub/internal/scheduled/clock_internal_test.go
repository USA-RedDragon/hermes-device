package scheduled

import (
	"testing"
	"time"
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestZoneLabelFollowsDST(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	if got := ZoneLabel(loc, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)); got != "Central (CST)" {
		t.Fatalf("November = %q", got)
	}
	if got := ZoneLabel(loc, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)); got != "Central (CDT)" {
		t.Fatalf("July = %q", got)
	}
	if got := ZoneLabel(time.UTC, time.Now()); got != "UTC" {
		t.Fatalf("UTC = %q", got)
	}
}

func TestWhenAndRelative(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, loc)
	next, ok := ParseTime("2026-11-17T01:00:00-06:00", loc)
	if !ok {
		t.Fatal("parse")
	}
	if got := When(next, loc); got != "Tue Nov 17, 1:00 AM" {
		t.Fatalf("When = %q", got)
	}
	// A UTC time shows on the Central clock.
	utc, _ := ParseTime("2026-11-12T14:30:00+00:00", loc)
	if got := When(utc, loc); got != "Thu Nov 12, 8:30 AM" {
		t.Fatalf("When(UTC) = %q", got)
	}
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(20 * time.Second), "now"},
		{now.Add(25 * time.Minute), "in 25 min"},
		{now.Add(6*time.Hour + 30*time.Minute), "in 6 h"},
		{time.Date(2026, 11, 12, 21, 0, 0, 0, loc), "tomorrow"},
		{time.Date(2026, 11, 13, 10, 0, 0, 0, loc), "in 2 days"},
		{next, "in 6 days"},
		{now.Add(-time.Hour), "overdue"},
	} {
		if got := Relative(now, c.at, loc); got != c.want {
			t.Errorf("Relative(%s) = %q, want %q", c.at, got, c.want)
		}
	}
}

func TestRelativeCountsCalendarDaysAcrossDST(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	// DST ended on Sun Nov 1, 2026: that day has 25 hours.
	now := time.Date(2026, 10, 31, 23, 0, 0, 0, loc)
	at := time.Date(2026, 11, 2, 23, 0, 0, 0, loc)
	if got := Relative(now, at, loc); got != "in 2 days" {
		t.Fatalf("across DST = %q", got)
	}
}

func TestParseTimeNaiveIsWallClock(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	got, ok := ParseTime("2026-11-12T09:00:00.123456", loc)
	if !ok || got.In(time.UTC).Hour() != 15 {
		t.Fatalf("naive = %v %v", got, ok)
	}
	if _, ok := ParseTime("not a time", loc); ok {
		t.Fatal("parsed garbage")
	}
	if _, ok := ParseTime("", loc); ok {
		t.Fatal("parsed empty")
	}
}

func TestDuration(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		31 * time.Second:               "31s",
		2*time.Minute + 41*time.Second: "2m 41s",
		64 * time.Minute:               "1h 04m",
		-time.Second:                   "",
		1500 * time.Millisecond:        "2s",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestNextRunsFollowHermesSchedule(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, loc)
	// A daily 9 AM job keeps 9 AM on the wall clock across the end of DST.
	runs, ok := nextRuns(schedule{Kind: kindCron, Expr: "0 9 * * *"}, now, loc, 4)
	if !ok || len(runs) != 4 {
		t.Fatalf("cron = %v %v", runs, ok)
	}
	for _, r := range runs {
		if r.In(loc).Hour() != 9 {
			t.Fatalf("cron run at %s", r.In(loc))
		}
	}
	if runs[1].Sub(runs[0]) != 25*time.Hour {
		t.Fatalf("DST day = %s", runs[1].Sub(runs[0]))
	}
	runs, ok = nextRuns(schedule{Kind: kindCron, Expr: "30 8 * * 1-5"}, time.Date(2026, 11, 13, 12, 0, 0, 0, loc), loc, 1)
	if !ok || When(runs[0], loc) != "Mon Nov 16, 8:30 AM" {
		t.Fatalf("weekdays = %v", runs)
	}
	runs, ok = nextRuns(schedule{Kind: kindInterval, Minutes: 360}, now, loc, 2)
	if !ok || runs[0] != now.Add(6*time.Hour) || runs[1] != now.Add(12*time.Hour) {
		t.Fatalf("interval = %v", runs)
	}
	runs, ok = nextRuns(schedule{Kind: kindOnce, RunAt: "2026-11-13T10:00:00-06:00"}, now, loc, 2)
	if !ok || len(runs) != 1 || When(runs[0], loc) != "Fri Nov 13, 10:00 AM" {
		t.Fatalf("once = %v", runs)
	}
	for _, s := range []schedule{{Kind: kindCron, Expr: "0 0 9 * * *"}, {Kind: kindCron, Expr: "bogus x y z w"}, {Kind: kindInterval}, {Kind: "what"}} {
		if _, ok := nextRuns(s, now, loc, 1); ok {
			t.Errorf("followed %+v", s)
		}
	}
}
