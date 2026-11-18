package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// A schedule is a watcher's `at`: the days of the week and the time of day its pass starts.
type schedule struct {
	days      [7]bool // by time.Weekday
	hour, min int
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// parseAt reads "17:45", for every day, or days and a time: "mon-fri 17:45", "mon,wed,sat 9:00".
// A range may wrap the week, as "fri-mon" does.
func parseAt(at string) (schedule, error) {
	var sc schedule
	fields := strings.Fields(strings.ToLower(at))
	if len(fields) == 0 || len(fields) > 2 {
		return sc, fmt.Errorf(`%q isn't a time ("17:45") or days and a time ("mon-fri 17:45")`, at)
	}
	t, err := time.Parse("15:04", fields[len(fields)-1])
	if err != nil {
		return sc, fmt.Errorf(`%q isn't a time ("17:45") or days and a time ("mon-fri 17:45")`, at)
	}
	sc.hour, sc.min = t.Hour(), t.Minute()
	if len(fields) == 1 {
		sc.days = [7]bool{true, true, true, true, true, true, true}
		return sc, nil
	}
	for part := range strings.SplitSeq(fields[0], ",") {
		from, to, ranged := strings.Cut(part, "-")
		first := slices.Index(dayNames, from)
		last := first
		if ranged {
			last = slices.Index(dayNames, to)
		}
		if first < 0 || last < 0 {
			return sc, fmt.Errorf("%q: %q isn't a day (mon, tue…) or a range of them (mon-fri)", at, part)
		}
		for d := first; ; d = (d + 1) % 7 {
			sc.days[d] = true
			if d == last {
				break
			}
		}
	}
	return sc, nil
}

// last is the schedule's latest time at or before now, in now's zone.
func (sc schedule) last(now time.Time) time.Time {
	// Eight days: when today's time is still ahead, the latest may be this weekday a week ago.
	for back := range 8 {
		t := time.Date(now.Year(), now.Month(), now.Day()-back, sc.hour, sc.min, 0, 0, now.Location())
		if sc.days[t.Weekday()] && !t.After(now) {
			return t
		}
	}
	return time.Time{}
}

// wakeIfDue calls wake when one of the schedule's times has passed since the one recorded for the
// watcher, and returns that time. Like wakeIfChanged it records after the wake, so a failed one is
// retried. Two cases are recorded without a wake: a watcher never polled before starts from its
// next time, and a time from an earlier day is skipped, since the day a late pass would report on
// is over.
func wakeIfDue(name string, sc schedule, now time.Time, wake func() error) (time.Time, error) {
	due := sc.last(now)
	ran, err := time.Parse(time.RFC3339, loadState().WatcherRan[name])
	if err == nil && !due.After(ran) {
		return time.Time{}, nil
	}
	y, m, d := now.Date()
	dy, dm, dd := due.Date()
	fire := err == nil && y == dy && m == dm && d == dd
	if fire {
		if err := wake(); err != nil {
			return time.Time{}, fmt.Errorf("waking it: %w", err)
		}
	}
	if _, err := updateState(func(s *State) { s.WatcherRan[name] = due.Format(time.RFC3339) }); err != nil || !fire {
		return time.Time{}, err
	}
	return due, nil
}
