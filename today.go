package main

// Today: the dispatches in progress, my tickets no agent is on yet, and what got done since
// yesterday, for the queue's today view and foreman today.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A todayRow is a line of one of today's three lists.
type todayRow struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"` // ticket, review, fix, task
	Title string `json:"title"`
	Repo  string `json:"repo,omitempty"`
	// State is what the queue's row says of a dispatch in progress, Status a ticket's in Jira, and
	// What how a dispatch got done, At being when.
	State  string     `json:"state,omitempty"`
	Status string     `json:"status,omitempty"`
	What   string     `json:"what,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	URL    string     `json:"url,omitempty"`
	// Dispatch is the dispatch's id in the event log, "" for one from before it.
	Dispatch string `json:"-"`
}

type todayList struct {
	Since      time.Time  `json:"since"`
	InProgress []todayRow `json:"inProgress"`
	UpNext     []todayRow `json:"upNext"`
	Done       []todayRow `json:"done"`
}

// sinceYesterday is the start of the day before now's.
func sinceYesterday(now time.Time) time.Time { return daysBack(now, 2) }

// keyKind is what a dispatch's key says it is.
func keyKind(key string) string {
	switch {
	case reviewKey(key):
		return "review"
	case isFix(key):
		return "fix"
	case isTask(key):
		return "task"
	}
	return "ticket"
}

// keyURL is the ticket a dispatch's key names, "" for a PR's or a task's.
func keyURL(key string) string {
	if strings.Contains(key, "#") || isTask(key) {
		return ""
	}
	return site + "/browse/" + key
}

// inProgress are the dispatches with something still going on: an open session, or a report or
// a question that waits on me. One whose PR merged, or that is over, is done whatever it shows.
func inProgress(s State, items []queueItem) []todayRow {
	rows := []todayRow{}
	for _, it := range items {
		d, ok := s.Dispatched[it.Key]
		if it.Kind != agentItem || !ok || it.Group == closed || d.Ended != "" || d.PrEnded == "merged" {
			continue
		}
		_, what := itemLabel(it)
		rows = append(rows, todayRow{Key: it.Key, Kind: keyKind(it.Key), Title: it.Title, Repo: it.Repo,
			State: ansi.Strip(what), URL: keyURL(it.Key), Dispatch: d.ID})
	}
	return rows
}

// notNext is in the status of a ticket whose work is past starting, or can't start: Blocked, In
// Review, Testing, In Staging.
var notNext = []string{"blocked", "review", "testing", "staging"}

// upNext are my open tickets no agent was dispatched on, in the board's order.
func upNext(s State, issues []Issue) []todayRow {
	rows := []todayRow{}
	for _, i := range issues {
		_, dispatched := s.Dispatched[i.Key]
		status := strings.ToLower(i.Status)
		if !i.Mine || dispatched || i.Category == "Done" || slices.ContainsFunc(notNext, func(w string) bool { return strings.Contains(status, w) }) {
			continue
		}
		rows = append(rows, todayRow{Key: i.Key, Kind: "ticket", Title: i.Summary, Status: i.Status, URL: keyURL(i.Key)})
	}
	return rows
}

// doneSince are the dispatches the log says got done since since, newest first: a ticket done,
// a PR of mine merged. One dispatch is one row, whatever of the two happened to it. A review's
// PR merging is someone else's work done.
func doneSince(s State, events []logEntry, since time.Time) []todayRow {
	type run struct{ key, dispatch string }
	started := map[run]logEntry{}
	prURL := map[string]string{}
	rows := map[run]*todayRow{}
	for _, e := range events {
		r := run{e.Key, e.Dispatch}
		what := ""
		switch e.Kind {
		case kindDispatched:
			started[r] = e
		case kindPrOpened:
			pr, url, _ := strings.Cut(e.Text, " ")
			prURL[pr] = url
		case "ticket-done":
			what = "ticket done"
		case "pr-merged":
			if !reviewKey(e.Key) {
				what = cmp.Or(e.Text, strings.TrimPrefix(e.Key, fixPrefix)) + " merged"
			}
		}
		if what == "" || e.At.Before(since) {
			continue
		}
		row := rows[r]
		if row == nil {
			row = &todayRow{Key: e.Key, Kind: keyKind(e.Key), URL: keyURL(e.Key), Dispatch: e.Dispatch}
			rows[r] = row
		}
		if row.What != "" {
			what = row.What + ", " + what
		}
		row.What = what
		if at := e.At; row.At == nil || at.After(*row.At) {
			row.At = &at
		}
		if row.URL == "" {
			row.URL = prURL[e.Text]
		}
	}
	out := []todayRow{}
	for r, row := range rows {
		d := s.Dispatched[r.key]
		if d.ID != r.dispatch {
			d = Dispatched{}
		}
		e := started[r]
		row.Title = cmp.Or(e.Title, d.Title)
		row.Repo = repoOf(r.key, cmp.Or(e.Dir, d.Dir))
		if e.Repo != "" {
			row.Repo = filepath.Base(e.Repo)
		}
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b todayRow) int { return cmp.Or(b.At.Compare(*a.At), strings.Compare(a.Key, b.Key)) })
	return out
}

// todayOf is today's three lists, from the state, the queue's items, the board's tickets and
// the event log.
func todayOf(s State, items []queueItem, issues []Issue, events []logEntry, now time.Time) todayList {
	since := sinceYesterday(now)
	return todayList{since, inProgress(s, items), upNext(s, issues), doneSince(s, events, since)}
}

func runToday(argv []string, out io.Writer) error {
	fs := newFlags("today")
	asJSON := fs.Bool("json", false, "")
	if _, err := parseFlags(fs, argv, 0); err != nil {
		return err
	}
	s, items, err := liveItems()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
	defer cancel()
	jira := &Jira{log: func(l string) { fmt.Fprintln(os.Stderr, l) }}
	defer jira.close()
	jql, _ := parseArgs(nil)
	// Without Jira the other two lists still stand.
	issues, _, err := jira.issues(ctx, jql)
	if err != nil {
		fmt.Fprintln(os.Stderr, "up next: Jira: "+firstLine(err.Error()))
	}
	t := todayOf(s, items, issues, readEvents(), time.Now())
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(t)
	}
	return writeToday(out, t)
}

// todayHead counts the three lists in a line.
func todayHead(t todayList) string {
	return fmt.Sprintf("%d in progress · %d up next · %d done since yesterday", len(t.InProgress), len(t.UpNext), len(t.Done))
}

// writeToday prints the three lists, a row a line: key, what it says, title.
func writeToday(out io.Writer, t todayList) error {
	lines := []string{todayHead(t)}
	keyW := 4
	for _, rows := range [][]todayRow{t.InProgress, t.UpNext, t.Done} {
		for _, r := range rows {
			keyW = max(keyW, len(r.Key))
		}
	}
	for _, l := range []struct {
		name string
		rows []todayRow
	}{{"in progress", t.InProgress}, {"up next", t.UpNext}, {"done since yesterday", t.Done}} {
		if len(l.rows) == 0 {
			continue
		}
		lines = append(lines, "", l.name)
		for _, r := range l.rows {
			lines = append(lines, fmt.Sprintf("  %-*s  %s  %s", keyW, r.Key, cmp.Or(r.State, r.Status, r.What), r.Title))
		}
	}
	_, err := fmt.Fprintln(out, strings.Join(lines, "\n"))
	return err
}

// timeline lists what the log has of one dispatch, oldest first, for a row's detail.
func timeline(events []logEntry, key, dispatch string) []string {
	out := []string{"## Timeline", ""}
	for _, e := range events {
		if e.Key != key || e.Dispatch != dispatch {
			continue
		}
		line := "- `" + e.At.Local().Format("2 Jan 15:04") + "` **" + e.Kind + "**"
		if e.By != "" {
			line += " by " + e.By
		}
		if text := firstLine(cmp.Or(e.Text, e.Title)); text != "" {
			line += ": " + text
		}
		out = append(out, line)
	}
	if len(out) == 2 {
		return nil
	}
	return out
}
