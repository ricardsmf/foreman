package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func rowKeys(rows []todayRow) string {
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	return strings.Join(keys, " ")
}

// Today's three lists: the dispatches with something still going on, my tickets no agent is on
// yet, and what the log says got done since yesterday.
func TestTodayLists(t *testing.T) {
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	yesterday, before := now.Add(-20*time.Hour), now.Add(-50*time.Hour)
	s := State{Dispatched: map[string]Dispatched{
		"FLD-1": {Title: "Working", ID: "a"}, "FLD-2": {Title: "Closed"}, "FLD-3": {Title: "Done", Ended: "done"},
		"FLD-4": {Title: "Merged", PrEnded: "merged"}, "task:app-1": {Title: "Reported"},
	}}
	items := []queueItem{
		{Kind: agentItem, Key: "FLD-1", Title: "Working", Group: working, Agent: "working", Repo: "app"},
		{Kind: agentItem, Key: "FLD-2", Title: "Closed", Group: closed},
		{Kind: agentItem, Key: "FLD-3", Title: "Done", Group: idle, Agent: "idle"},
		{Kind: agentItem, Key: "FLD-4", Title: "Merged", Group: idle, Agent: "idle"},
		{Kind: agentItem, Key: "task:app-1", Title: "Reported", Group: toReview, Report: "/r/report.md"},
		{Kind: agentItem, Key: "fld-pr-watch", Watcher: true, Group: idle, Agent: "idle"},
		{Kind: prItem, Key: "app#3", Group: waiting, Pr: &Pr{Key: "app#3"}},
	}
	issues := []Issue{
		{Key: "FLD-10", Summary: "Ten", Status: "In Progress", Category: "In Progress", Mine: true},
		{Key: "FLD-11", Summary: "Eleven", Status: "To Do", Category: "To Do", Mine: true},
		{Key: "FLD-1", Summary: "Working", Status: "In Progress", Mine: true},
		{Key: "FLD-12", Summary: "Someone else's", Status: "To Do"},
		{Key: "FLD-13", Summary: "In review", Status: "In Review", Mine: true},
		{Key: "FLD-14", Summary: "In staging", Status: "In Staging", Mine: true},
		{Key: "FLD-15", Summary: "Blocked", Status: "BLOCKED", Mine: true},
		{Key: "FLD-16", Summary: "Done", Status: "Closed", Category: "Done", Mine: true},
	}
	events := []logEntry{
		{At: before, Key: "FLD-9", Dispatch: "a", Kind: kindDispatched, Title: "Nine", Dir: "/w/app-fld-9", Repo: "/w/app"},
		{At: before, Key: "FLD-9", Dispatch: "a", Kind: kindPrOpened, Text: "app#5 https://github.com/o/app/pull/5"},
		{At: yesterday, Key: "FLD-9", Dispatch: "a", Kind: "pr-merged", Text: "app#5"},
		{At: now.Add(-time.Hour), Key: "FLD-9", Dispatch: "a", Kind: "ticket-done"},
		{At: before, Key: "FLD-8", Dispatch: "a", Kind: "ticket-done"},
		{At: before, Key: "task:app-7", Dispatch: "a", Kind: kindPrOpened, Text: "app#6 https://github.com/o/app/pull/6"},
		{At: now.Add(-2 * time.Hour), Key: "task:app-7", Dispatch: "a", Kind: "pr-merged", Text: "app#6"},
		{At: now.Add(-3 * time.Hour), Key: "fix:app#7", Dispatch: "a", Kind: "pr-merged"},
		{At: now.Add(-3 * time.Hour), Key: "app#8", Dispatch: "a", Kind: "pr-merged"},
		{At: now.Add(-3 * time.Hour), Key: "FLD-7", Dispatch: "a", Kind: "pr-closed", Text: "app#9"},
	}
	got := todayOf(s, items, issues, events, now)
	if !got.Since.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("since %s; want the start of yesterday", got.Since)
	}
	if keys := rowKeys(got.InProgress); keys != "FLD-1 task:app-1" {
		t.Errorf("in progress: %s; want the working agent and the report to read", keys)
	}
	if r := got.InProgress[0]; r.State != "working" || r.Kind != "ticket" || r.Repo != "app" || r.Dispatch != "a" || r.URL != site+"/browse/FLD-1" {
		t.Errorf("FLD-1's row: %+v", r)
	}
	if keys := rowKeys(got.UpNext); keys != "FLD-10 FLD-11" {
		t.Errorf("up next: %s; want my open tickets no agent is on, not past starting", keys)
	}
	if keys := rowKeys(got.Done); keys != "FLD-9 task:app-7 fix:app#7" {
		t.Fatalf("done: %s; want what got done since yesterday, newest first, a review's merge aside", keys)
	}
	nine, task, fix := got.Done[0], got.Done[1], got.Done[2]
	if nine.What != "app#5 merged, ticket done" || nine.Title != "Nine" || nine.Repo != "app" || !nine.At.Equal(now.Add(-time.Hour)) {
		t.Errorf("FLD-9's row, one for its PR and its ticket: %+v", nine)
	}
	if task.What != "app#6 merged" || task.URL != "https://github.com/o/app/pull/6" || task.Kind != "task" {
		t.Errorf("the task's row: %+v", task)
	}
	if fix.What != "app#7 merged" || fix.Kind != "fix" {
		t.Errorf("the fix's row: %+v", fix)
	}

	var out bytes.Buffer
	if err := writeToday(&out, got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 in progress · 2 up next · 3 done since yesterday", "FLD-1       working  Working", "FLD-10      In Progress  Ten", "FLD-9       app#5 merged, ticket done  Nine"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("foreman today lacks %q:\n%s", want, out.String())
		}
	}
}

// A ticket's or a task's own PR that is no longer open is asked about once it is gone, and how
// it ended is logged once, at the time GitHub gives.
func TestOwnPrEndLoggedOnce(t *testing.T) {
	tempState(t)
	mergedAt := time.Date(2026, 11, 22, 18, 30, 0, 0, time.UTC)
	asked := 0
	oldEnd := prEnd
	prEnd = func(dir, num string) (string, time.Time, error) {
		asked++
		switch num {
		case "3":
			return "MERGED", mergedAt, nil
		case "4":
			return "OPEN", time.Time{}, nil
		}
		return "", time.Time{}, errors.New("gh: not found")
	}
	t.Cleanup(func() { prEnd = oldEnd })
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"] = Dispatched{Dir: "/w/1", ID: "a", Pr: "app#3"}
		s.Dispatched["FLD-2"] = Dispatched{Dir: "/w/2", ID: "a", Pr: "app#4"}
		s.Dispatched["FLD-3"] = Dispatched{Dir: "/w/3", ID: "a", Pr: "app#5"}
		s.Dispatched["FLD-4"] = Dispatched{Dir: "/w/4", ID: "a", Pr: "app#6"}
		s.Dispatched["FLD-5"] = Dispatched{Dir: "/w/5", ID: "a"}
	}); err != nil {
		t.Fatal(err)
	}
	open := []Pr{{Key: "app#6"}}
	gone := goneOwnPrs(loadState(), open)
	if len(gone) != 3 || gone["FLD-4"].Dir != "" || gone["FLD-5"].Dir != "" {
		t.Fatalf("gone: %v; want the three whose PR isn't open", gone)
	}
	noteOwnPrsEnded(gone)
	noteOwnPrsEnded(gone)
	merged := eventsOf(loggedEvents(t), "pr-merged")
	if len(merged) != 1 || merged[0]["key"] != "FLD-1" || merged[0]["text"] != "app#3" || merged[0]["dispatch"] != "a" || merged[0]["at"] != "2026-11-22T18:30:00Z" {
		t.Errorf("pr-merged events: %v; want FLD-1's, once, at the merge", merged)
	}
	if n := len(loggedEvents(t)); n != 1 {
		t.Errorf("%d events; want none for a PR still open or one GitHub can't find", n)
	}
	s := loadState()
	if s.Dispatched["FLD-1"].PrEnded != "merged" || s.Dispatched["FLD-2"].PrEnded != "" {
		t.Errorf("recorded ends: %q and %q", s.Dispatched["FLD-1"].PrEnded, s.Dispatched["FLD-2"].PrEnded)
	}
	// Logged, it is asked about no more; the two that couldn't be told are.
	if again := goneOwnPrs(s, open); len(again) != 2 || again["FLD-1"].Dir != "" {
		t.Errorf("gone after: %v", again)
	}
}

// A dispatched ticket that is done is logged so once, whatever its agent's session shows.
func TestTicketsDoneLogged(t *testing.T) {
	tempState(t)
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"] = Dispatched{Dir: "/w/1", ID: "a"}
		s.Dispatched["FLD-2"] = Dispatched{Dir: "/w/2", ID: "a"}
	}); err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{"FLD-1": "Done", "FLD-2": "In Progress"}
	noteTicketsDone(t.Context(), nil, listed)
	noteTicketsDone(t.Context(), nil, listed)
	if done := eventsOf(loggedEvents(t), "ticket-done"); len(done) != 1 || done[0]["key"] != "FLD-1" {
		t.Errorf("ticket-done events: %v; want FLD-1's, once", done)
	}
}

// tab turns the queue to today and to the stats; a row of today goes to its agent, to the
// board's dispatch form or to a browser, and the queue's own keys don't reach a row out of sight.
func TestQueueTabs(t *testing.T) {
	useFakeRex(t, rexSession{ID: "s1", Label: "fld-1", State: "working"})
	tempState(t)
	tempHome(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session, d.ID = "s1", "a" }) })
	if err := writeEvents(
		logEntry{Key: "FLD-1", Dispatch: "a", By: byMe, Kind: kindDispatched, Title: "One"},
		logEntry{Key: "FLD-9", Dispatch: "a", By: byForeman, Kind: "ticket-done"},
	); err != nil {
		t.Fatal(err)
	}
	q := &queueModel{details: map[string][]string{}, width: 100, height: 30, state: loadState(),
		sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "working"}}}
	q.rebuild()
	q.Update(queueTicketsMsg{[]Issue{{Key: "FLD-7", Summary: "Seven", Status: "To Do", Mine: true}}})
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	q.input(tab)
	screen := ansi.Strip(q.content())
	for _, want := range []string{"today", "1 in progress", "1 up next", "1 done since yesterday", "progress", "FLD-1", "up next", "FLD-7", "to do", "ticket done", "Timeline", "dispatched by me: One"} {
		if !strings.Contains(screen, want) {
			t.Errorf("today lacks %q:\n%s", want, screen)
		}
	}
	if n := len(strings.Split(q.content(), "\n")); n != 30 {
		t.Errorf("today is %d lines; want the pane's 30", n)
	}
	// f would type a reply to the agent under the queue's cursor, which isn't showing.
	if q.input(tea.KeyPressMsg{Code: 'f', Text: "f"}); q.feedback != nil {
		t.Error("f on today opened a reply")
	}
	q.input(tea.KeyPressMsg{Code: tea.KeyDown})
	if msg, ok := q.input(tea.KeyPressMsg{Code: tea.KeyEnter})().(openFormMsg); !ok || msg.key != "FLD-7" {
		t.Errorf("enter on a ticket up next: %v; want the dispatch form on FLD-7", msg)
	}
	q.input(tea.KeyPressMsg{Code: tea.KeyDown})
	if screen := ansi.Strip(q.content()); !strings.Contains(screen, "Done: ticket done") {
		t.Errorf("the done row's detail:\n%s", screen)
	}

	// The stats count off the screen's thread, and show when the count is of the days asked for.
	count := q.input(tab)
	if q.tab != statsTab || count == nil || !q.statsLoading {
		t.Fatalf("tab from today: view %d, loading %t", q.tab, q.statsLoading)
	}
	q.Update(count())
	screen = ansi.Strip(q.content())
	for _, want := range []string{"stats", "today  7 days  30 days", "by ticket", "$0.00", "0 tokens", "1 ticket done"} {
		if !strings.Contains(screen, want) {
			t.Errorf("stats lack %q:\n%s", want, screen)
		}
	}
	if n := len(strings.Split(q.content(), "\n")); n != 30 {
		t.Errorf("stats are %d lines; want the pane's 30", n)
	}
	week := q.input(tea.KeyPressMsg{Code: tea.KeyRight})
	if q.statsDays != 7 || week == nil {
		t.Fatalf("→: %d days", q.statsDays)
	}
	// A count that lands after I moved on to another grouping is counted again.
	q.input(tea.KeyPressMsg{Code: 'g', Text: "g"})
	if _, again := q.Update(week()); again == nil || q.stats.Days != 1 {
		t.Errorf("a stale count: shown for %d days, counted again %t", q.stats.Days, again != nil)
	}
	if q.input(tab); q.tab != listTab || !strings.Contains(ansi.Strip(q.content()), "queue") {
		t.Errorf("tab from the stats: view %d", q.tab)
	}
}

// The board's tickets reach the queue, for today's up next.
func TestAppHandsTicketsToTheQueue(t *testing.T) {
	a := testApp(t, false)
	a.Update(loadedMsg{which: tickets, rows: []Row{issueRow(Issue{Key: "FLD-8", Summary: "Eight", Mine: true})}})
	if len(a.queue.issues) != 1 || a.queue.issues[0].Key != "FLD-8" {
		t.Errorf("the queue's tickets: %+v", a.queue.issues)
	}
	a.Update(loadedMsg{which: tickets, err: errors.New("jira is down")})
	if len(a.queue.issues) != 1 {
		t.Error("a failed load emptied the queue's tickets")
	}
}
