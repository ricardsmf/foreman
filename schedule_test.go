package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// 16 November 2026 is a Monday.
func nov(day, hour, min int) time.Time {
	return time.Date(2026, time.November, day, hour, min, 0, 0, time.FixedZone("WET", 0))
}

func TestParseAt(t *testing.T) {
	days := func(sc schedule) string {
		var on []string
		for d, name := range dayNames {
			if sc.days[d] {
				on = append(on, name)
			}
		}
		return strings.Join(on, ",")
	}
	for at, want := range map[string]string{
		"17:45":            "sun,mon,tue,wed,thu,fri,sat 17:45",
		"mon-fri 9:05":     "mon,tue,wed,thu,fri 9:05",
		"Mon-Fri 17:45":    "mon,tue,wed,thu,fri 17:45",
		"sat,sun 10:00":    "sun,sat 10:00",
		"fri-mon 08:00":    "sun,mon,fri,sat 8:00",
		"mon,wed-thu 0:00": "mon,wed,thu 0:00",
	} {
		sc, err := parseAt(at)
		if err != nil {
			t.Errorf("%q: %v", at, err)
			continue
		}
		if got := fmt.Sprintf("%s %d:%02d", days(sc), sc.hour, sc.min); got != want {
			t.Errorf("%q: %s, want %s", at, got, want)
		}
	}
	for _, at := range []string{"", "25:00", "17:45 mon-fri", "weekdays 17:45", "mon- 17:45", "mon-fri 17:45 daily", "5pm"} {
		if _, err := parseAt(at); err == nil {
			t.Errorf("%q should be refused", at)
		}
	}
}

// The latest time at or before now: today's once it has passed, else the last day the schedule names.
func TestScheduleLast(t *testing.T) {
	weekdays, _ := parseAt("mon-fri 17:45")
	tuesdays, _ := parseAt("tue 10:00")
	for _, c := range []struct {
		what string
		sc   schedule
		now  time.Time
		want time.Time
	}{
		{"after today's", weekdays, nov(17, 18, 0), nov(17, 17, 45)},
		{"on the minute", weekdays, nov(17, 17, 45), nov(17, 17, 45)},
		{"before today's", weekdays, nov(17, 9, 0), nov(16, 17, 45)},
		{"Monday morning", weekdays, nov(16, 9, 0), nov(13, 17, 45)},
		{"the weekend", weekdays, nov(15, 12, 0), nov(13, 17, 45)},
		{"a week ago, before today's", tuesdays, nov(17, 9, 0), nov(10, 10, 0)},
	} {
		if got := c.sc.last(c.now); !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", c.what, got.Format("Mon 2 15:04"), c.want.Format("Mon 2 15:04"))
		}
	}
}

// A scheduled watcher is woken once per time, from the first one after its first poll. A failed
// wake is retried, a time missed on an earlier day is skipped, and one missed today still runs.
func TestWakeIfDue(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })

	sc, _ := parseAt("mon-fri 17:45")
	wakes := 0
	wake := func() error { wakes++; return nil }
	poll := func(what string, now time.Time, wantWakes int) {
		t.Helper()
		before := wakes
		at, err := wakeIfDue("eod", sc, now, wake)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		// It returns the time it woke the watcher for, and none without a wake.
		if wakes != wantWakes || at.IsZero() != (wakes == before) {
			t.Fatalf("%s: %d wakes, want %d (returned %v)", what, wakes, wantWakes, at)
		}
	}
	poll("never polled before, yesterday's time passed", nov(17, 17, 0), 0)
	poll("its time came", nov(17, 17, 46), 1)
	poll("the same time again", nov(17, 17, 48), 1)

	if _, err := wakeIfDue("eod", sc, nov(18, 17, 46), func() error { return errors.New("no pane") }); err == nil {
		t.Fatal("a failed wake should say so")
	}
	if got := loadState().WatcherRan["eod"]; got != nov(17, 17, 45).Format(time.RFC3339) {
		t.Fatalf("a failed wake kept the new record: %s", got)
	}
	poll("retry after a failed wake", nov(18, 17, 48), 2)

	poll("Thursday's time missed, foreman watch back on Friday morning", nov(20, 9, 0), 2)
	if got := loadState().WatcherRan["eod"]; got != nov(19, 17, 45).Format(time.RFC3339) {
		t.Errorf("the skipped time wasn't recorded: %s", got)
	}
	poll("Friday's own time", nov(20, 17, 46), 3)
	poll("missed by hours, the same day", nov(23, 21, 0), 4)

	// Moving the time earlier in the day doesn't run a second pass that day.
	sc, _ = parseAt("mon-fri 9:00")
	poll("the schedule moved earlier", nov(23, 21, 30), 4)
	poll("the next morning", nov(24, 9, 1), 5)
}

// A scheduled watcher that is busy or not running is left for the next poll; at rest, its pass
// starts on an empty conversation, with the rules every pass gets.
func TestPollSchedule(t *testing.T) {
	f := useFakeRex(t)
	w := Watcher{Name: "eod", Dir: "/r/eod", Prompt: "Write my update.", At: "mon-fri 17:45"}
	if _, err := updateState(func(s *State) { s.WatcherRan["eod"] = nov(16, 17, 45).Format(time.RFC3339) }); err != nil {
		t.Fatal(err)
	}
	session := func(state string) []rexSession {
		return []rexSession{{ID: "s1", Label: "eod", State: state, Block: "b1"}}
	}
	for _, state := range []string{"working", "blocked", "open"} {
		if at, err := pollSchedule(w, session(state), "s1", nov(17, 17, 46)); !at.IsZero() || err != nil {
			t.Errorf("%s: %v %v", state, at, err)
		}
	}
	if at, err := pollSchedule(w, nil, "s1", nov(17, 17, 46)); !at.IsZero() || err != nil {
		t.Errorf("not running: %v %v", at, err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("typed into a watcher that wasn't at rest: %q", f.calls)
	}
	at, err := pollSchedule(w, session("idle"), "s1", nov(17, 17, 50))
	if err != nil || !at.Equal(nov(17, 17, 45)) {
		t.Fatalf("at rest after its time: %v %v", at, err)
	}
	want := []string{"send s1 b1 /clear", "press s1 b1 enter", "send s1 b1 Write my update. " + watcherRules, "press s1 b1 enter"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %q", f.calls)
	}
}

// Starting a scheduled watcher isn't its time: it starts at its prompt, without a pass.
func TestScheduledWatcherStartsWithoutAPass(t *testing.T) {
	f := useFakeRex(t)
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{Name: "eod", Dir: t.TempDir(), Prompt: "Write my update.", At: "17:45"}}
	started, err := startWatchers()
	if err != nil || !slices.Equal(started, []string{"eod"}) {
		t.Fatalf("started %v: %v", started, err)
	}
	if cmd := f.cmds["eod"]; !slices.Equal(cmd, watcherCommand("")) {
		t.Errorf("command %q, want one without a prompt", cmd)
	}
}

func TestScheduledWatcherConfig(t *testing.T) {
	oldFile := configFile
	t.Cleanup(func() { configFile, watchers = oldFile, nil })
	configFile = filepath.Join(t.TempDir(), "config.json")
	write := func(body string) error {
		if err := os.WriteFile(configFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadConfig()
	}
	if err := write(`{"watchers": [{"name": "eod", "dir": "/x", "prompt": "/eod", "at": "mon-fri 17:45"}]}`); err != nil || watchers[0].At != "mon-fri 17:45" {
		t.Errorf("a scheduled watcher: %v, %+v", err, watchers)
	}
	err := write(`{"watchers": [{"name": "eod", "dir": "/x", "prompt": "/eod", "at": "teatime"}]}`)
	if err == nil || !strings.Contains(err.Error(), "teatime") {
		t.Errorf("a time that isn't one: %v", err)
	}
	// With prs too, the time is a sweep beside the wakes its PRs cause.
	if err := write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/sweep", "at": "9:00", "prs": ["is:open"]}]}`); err != nil {
		t.Errorf("at with prs: %v", err)
	}
	err = write(`{"watchers": [{"name": "eod", "dir": "/x", "prompt": "/loop 1d /eod", "at": "17:45"}]}`)
	if err == nil || !strings.Contains(err.Error(), "/loop") {
		t.Errorf("a scheduled watcher on a loop: %v", err)
	}
}

// A scheduled watcher between passes says when its next one is due.
func TestScheduledWatcherLabel(t *testing.T) {
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{Name: "eod", Dir: "/r/eod", Prompt: "/eod", At: "mon-fri 17:45"}}
	items := queueItems(State{}, []rexSession{{ID: "s1", Label: "eod", State: "idle"}}, nil)
	if _, label := itemLabel(items[0]); !strings.Contains(label, "waits for mon-fri 17:45") {
		t.Errorf("label %q", label)
	}
	watchers[0].PRs = []string{"is:open"}
	items = queueItems(State{}, []rexSession{{ID: "s1", Label: "eod", State: "idle"}}, nil)
	if _, label := itemLabel(items[0]); !strings.Contains(label, "waits for a PR to change, or mon-fri 17:45") {
		t.Errorf("with prs: label %q", label)
	}
}

// A watcher with a schedule and PRs is polled for both: its time comes first, and a pass it
// starts is the whole poll.
func TestPollOneSchedulesBeforePRs(t *testing.T) {
	f := useFakeRex(t)
	w := Watcher{Name: "w", Dir: "/nonexistent/watch", Prompt: "/sweep", At: time.Now().Add(-time.Minute).Format("15:04"), PRs: []string{"is:open"}}
	if _, err := updateState(func(s *State) { s.WatcherRan["w"] = time.Now().Add(-24 * time.Hour).Format(time.RFC3339) }); err != nil {
		t.Fatal(err)
	}
	woke, err := pollOne(w, []rexSession{{ID: "s1", Label: "w", State: "idle", Block: "b1"}}, "s1")
	if err != nil || !strings.HasPrefix(woke, "Woke w for ") {
		t.Fatalf("woke %q: %v", woke, err)
	}
	if len(f.calls) != 4 {
		t.Errorf("calls %q", f.calls)
	}
}
