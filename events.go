package main

// The event log: one line per thing that happened in a dispatch, from its start to its cleanup,
// beside the state file. Nothing removes it, so a cleanup or the key's next dispatch keeps what
// happened before.

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// A logEntry is one line of the event log. Dispatch tells apart the runs of one key: a key is
// dispatched again with --again or after a cleanup. A dispatch from before the log has none.
type logEntry struct {
	At       time.Time `json:"at"`
	Key      string    `json:"key,omitempty"`
	Dispatch string    `json:"dispatch,omitempty"`
	By       string    `json:"by"`   // agent, orchestrator, me, foreman
	Kind     string    `json:"kind"` // see the kinds below
	Text     string    `json:"text,omitempty"`
	// What a dispatched event records of the dispatch.
	Title       string `json:"title,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Dir         string `json:"dir,omitempty"`
	Repo        string `json:"repo,omitempty"`
	StartedFrom string `json:"startedFrom,omitempty"`
}

// byForeman is foreman itself, noting what it saw: a report, a PR, a ticket, a worktree gone.
const byForeman = "foreman"

// The kinds of the events that aren't questions and what answers them.
const (
	kindDispatched      = "dispatched"
	kindReported        = "reported"
	kindReportRewritten = "report-rewritten"
	kindPrOpened        = "pr-opened"
	kindCleanedUp       = "cleaned-up"
)

// questionKinds are the entries the queue's Questions section lists.
var questionKinds = []string{"question", "decision", "answer", "confirm", "aside", "reply", "expired"}

// endedKinds name the event for each of finished's reasons.
var endedKinds = map[string]string{"merged": "pr-merged", "closed": "pr-closed", "done": "ticket-done"}

func eventsFile() string { return filepath.Join(filepath.Dir(stateFile), "events.jsonl") }

// newDispatchID names a dispatch by when it started, to the millisecond.
func newDispatchID() string { return time.Now().UTC().Format("20060102T150405.000Z") }

// withEvents runs fn on the event log opened for appending, holding an exclusive lock on it: foreman
// ask, the queue and foreman watch are separate processes that append at once.
func withEvents(fn func(f *os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(eventsFile()), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(eventsFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	return fn(f)
}

// writeEvents appends the events in one write.
func writeEvents(es ...logEntry) error {
	if len(es) == 0 {
		return nil
	}
	var b []byte
	for _, e := range es {
		if e.At.IsZero() {
			e.At = time.Now()
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b = append(append(b, line...), '\n')
	}
	return withEvents(func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
}

// appendLog logs e in key's current dispatch.
func appendLog(key string, e logEntry) error {
	e.Key, e.Dispatch = key, loadState().Dispatched[key].ID
	return writeEvents(e)
}

// readLog is the questions and answers of key's current dispatch, oldest first: those logged, after
// those a dispatch from before the log kept in its evidence.
func readLog(key string) []logEntry {
	id := loadState().Dispatched[key].ID
	var out []logEntry
	if id == "" {
		out = readEntries(legacyLogFile(key), func(logEntry) bool { return true })
	}
	return append(out, readEntries(eventsFile(), func(e logEntry) bool {
		return e.Key == key && e.Dispatch == id && slices.Contains(questionKinds, e.Kind)
	})...)
}

func readEntries(file string, keep func(logEntry) bool) []logEntry {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []logEntry
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		var e logEntry
		if len(line) > 0 && json.Unmarshal(line, &e) == nil && keep(e) {
			out = append(out, e)
		}
		if err != nil {
			return out
		}
	}
}

// legacyLogFile is where a dispatch from before the log kept its questions, in its evidence.
func legacyLogFile(key string) string { return filepath.Join(evidenceDir(key), "questions.jsonl") }

// keepLegacyLog moves key's questions.jsonl into the log, as its dispatch id's, before its evidence
// goes. Under the log's lock, so two processes don't both move it.
func keepLegacyLog(key, id string) error {
	return withEvents(func(f *os.File) error {
		entries := readEntries(legacyLogFile(key), func(logEntry) bool { return true })
		var b []byte
		for _, e := range entries {
			e.Key, e.Dispatch = key, id
			line, _ := json.Marshal(e)
			b = append(append(b, line...), '\n')
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
		if err := os.Remove(legacyLogFile(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// reportTitle is the outcome a report's first line states.
func reportTitle(file string) string {
	b, _ := os.ReadFile(file)
	first, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(strings.TrimLeft(first, "# "))
}

// noteReports logs each dispatch's report the first time it is seen, and again when it is rewritten.
// The queue and foreman watch both call it on every poll: the time of the report logged, kept in the
// dispatch, makes each change logged once whoever sees it first. It returns the state as updated.
func noteReports(s State) State {
	seen := map[string]time.Time{}
	for key, d := range s.Dispatched {
		st, err := os.Stat(filepath.Join(evidenceDir(key), "report.md"))
		if err == nil && st.ModTime().After(d.Reported) {
			seen[key] = st.ModTime()
		}
	}
	if len(seen) == 0 {
		return s
	}
	var events []logEntry
	updated, err := updateState(func(s *State) {
		for key, at := range seen {
			d, ok := s.Dispatched[key]
			if !ok || !at.After(d.Reported) {
				continue
			}
			kind := kindReported
			if !d.Reported.IsZero() {
				kind = kindReportRewritten
			}
			events = append(events, logEntry{At: at, Key: key, Dispatch: d.ID, By: byAgent, Kind: kind,
				Text: reportTitle(filepath.Join(evidenceDir(key), "report.md"))})
			s.change(key, func(d *Dispatched) { d.Reported = at })
		}
	})
	if err != nil {
		return s
	}
	slices.SortFunc(events, func(a, b logEntry) int { return a.At.Compare(b.At) })
	_ = writeEvents(events...)
	return updated
}

// notePrs logs, once each, the PR of mine that a ticket's or task's dispatch opened: the one on the
// branch its worktree has checked out, in its repo. A review's or a fix's PR was there before it.
func notePrs(s State, prs []Pr) State {
	seen := map[string]Pr{}
	for key, d := range s.Dispatched {
		if strings.Contains(key, "#") {
			continue
		}
		if p, ok := prOn(prs, d.Dir); ok && p.Key != d.Pr {
			seen[key] = p
		}
	}
	if len(seen) == 0 {
		return s
	}
	var events []logEntry
	updated, err := updateState(func(s *State) {
		for key, p := range seen {
			if d, ok := s.Dispatched[key]; ok && d.Pr != p.Key {
				events = append(events, logEntry{Key: key, Dispatch: d.ID, By: byForeman, Kind: kindPrOpened, Text: p.Key + " " + p.URL})
				s.change(key, func(d *Dispatched) { d.Pr = p.Key })
			}
		}
	})
	if err != nil {
		return s
	}
	_ = writeEvents(events...)
	return updated
}

// noteEnded logs, once per dispatch, why key's dispatch is over: its PR merged or closed, its
// ticket done, as finished tells.
func noteEnded(key, reason string) {
	kind := endedKinds[reason]
	if kind == "" {
		return
	}
	var event *logEntry
	_, err := updateState(func(s *State) {
		if d, ok := s.Dispatched[key]; ok && d.Ended != reason {
			event = &logEntry{Key: key, Dispatch: d.ID, By: byForeman, Kind: kind}
			s.change(key, func(d *Dispatched) { d.Ended = reason })
		}
	})
	if err == nil && event != nil {
		_ = writeEvents(*event)
	}
}
