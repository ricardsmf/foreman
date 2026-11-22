package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loggedEvents reads the event log beside the state file, failing on a line that isn't whole JSON.
func loggedEvents(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(filepath.Dir(stateFile), "events.jsonl"))
	if err != nil {
		t.Fatalf("the event log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			break
		}
		var e map[string]any
		if json.Unmarshal(line, &e) != nil || line[len(line)-1] != '\n' {
			t.Fatalf("line %d isn't whole: %.80q…", n, line)
		}
		out = append(out, e)
	}
	return out
}

func eventsOf(evs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["kind"] == kind {
			out = append(out, e)
		}
	}
	return out
}

// What happened in a dispatch stays in the log through its cleanup and the key's next dispatch,
// and each line names the dispatch it belongs to.
func TestEventSurvivesDiscardAndRedispatch(t *testing.T) {
	tempState(t)
	dir := t.TempDir()
	markDispatched("FLD-1", "One", dir, "s1", agentPi, "abc")
	if err := appendLog("FLD-1", logEntry{By: byAgent, Kind: "question", Text: "Which store?"}); err != nil {
		t.Fatal(err)
	}
	discard(map[string]string{"FLD-1": "done"}, byMe)
	if err := resetEvidence("FLD-1"); err != nil {
		t.Fatal(err)
	}
	markDispatched("FLD-1", "One again", dir, "s2", agentPi, "def")

	evs := loggedEvents(t)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, fmt.Sprint(e["kind"]))
	}
	if got := strings.Join(kinds, " "); got != "dispatched question cleaned-up dispatched" {
		t.Fatalf("kinds = %q", got)
	}
	first, second := evs[0], evs[3]
	if first["dispatch"] == "" || first["dispatch"] == second["dispatch"] {
		t.Errorf("dispatches %v and %v aren't told apart", first["dispatch"], second["dispatch"])
	}
	if evs[1]["dispatch"] != first["dispatch"] || evs[1]["key"] != "FLD-1" || evs[1]["text"] != "Which store?" {
		t.Errorf("the question: %v", evs[1])
	}
	if first["title"] != "One" || first["agent"] != agentPi || first["dir"] != dir || first["startedFrom"] != "abc" {
		t.Errorf("dispatched: %v", first)
	}
}

// The queue's Questions section is the current dispatch's: an earlier run's stays in the log only.
func TestQuestionsShowOnlyTheLatestDispatch(t *testing.T) {
	tempState(t)
	dir := t.TempDir()
	markDispatched("FLD-1", "One", dir, "s1", agentPi, "")
	_ = appendLog("FLD-1", logEntry{By: byAgent, Kind: "question", Text: "First run?"})
	if err := resetEvidence("FLD-1"); err != nil {
		t.Fatal(err)
	}
	markDispatched("FLD-1", "One", dir, "s2", agentPi, "")
	_ = appendLog("FLD-1", logEntry{By: byAgent, Kind: "question", Text: "Second run?"})

	detail := strings.Join(itemDetail(queueItems(loadState(), nil, nil)[0]), "\n")
	if !strings.Contains(detail, "Second run?") || strings.Contains(detail, "First run?") {
		t.Errorf("detail:\n%s", detail)
	}
	if len(eventsOf(loggedEvents(t), "question")) != 2 {
		t.Error("the earlier run's question left the log")
	}
}

// A dispatch from before the log keeps its questions.jsonl: they show beside what is logged since,
// and its cleanup moves them into the log.
func TestLegacyQuestionsStillShow(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", t.TempDir())
	old, _ := json.Marshal(logEntry{At: time.Unix(1700000000, 0), By: byAgent, Kind: "question", Text: "Asked before the log?"})
	if err := os.WriteFile(filepath.Join(evidenceDir("FLD-1"), "questions.jsonl"), append(old, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = appendLog("FLD-1", logEntry{By: byMe, Kind: "answer", Text: "Yes"})
	detail := strings.Join(itemDetail(queueItems(loadState(), nil, nil)[0]), "\n")
	if !strings.Contains(detail, "Asked before the log?") || !strings.Contains(detail, "Yes") {
		t.Errorf("detail:\n%s", detail)
	}
	discard(map[string]string{"FLD-1": "done"}, byMe)
	if q := eventsOf(loggedEvents(t), "question"); len(q) != 1 || q[0]["text"] != "Asked before the log?" || q[0]["key"] != "FLD-1" {
		t.Errorf("after cleanup, the log's questions: %v", q)
	}
}

// foreman ask, the queue and foreman watch append from separate processes: no line is torn.
func TestEventsAppendWholeAcrossProcesses(t *testing.T) {
	const writers, each = 4, 25
	if name := os.Getenv("FOREMAN_TEST_EVENT_WRITER"); name != "" {
		stateFile, evidenceRoot = os.Getenv("FOREMAN_TEST_STATE_FILE"), os.Getenv("FOREMAN_TEST_EVIDENCE")
		long := strings.Repeat(name, 64*1024/len(name))
		for n := range each {
			if err := appendLog("FLD-1", logEntry{By: byAgent, Kind: "reply", Text: fmt.Sprint(n, long)}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	tempState(t)
	dispatched(t, "FLD-1", t.TempDir())
	var cmds []*exec.Cmd
	for n := range writers {
		cmd := exec.Command(os.Args[0], "-test.run=^TestEventsAppendWholeAcrossProcesses$")
		cmd.Env = append(os.Environ(), fmt.Sprintf("FOREMAN_TEST_EVENT_WRITER=W%d", n), "FOREMAN_TEST_STATE_FILE="+stateFile, "FOREMAN_TEST_EVIDENCE="+evidenceRoot)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(eventsOf(loggedEvents(t), "reply")); got != writers*each {
		t.Errorf("%d of %d lines logged", got, writers*each)
	}
}

// A PR on a dispatch's branch is logged as opened once, on every load of the PRs that lists it,
// and its end once, however often the board checks it.
func TestPrAndEndLoggedOnce(t *testing.T) {
	tempState(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/feat\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "feat"), []byte("abc\n"), 0o644)
	markDispatched("FLD-1", "One", dir, "s1", agentPi, "")
	markDispatched("app#5", "A review", dir, "s2", agentPi, "")
	prs := []Pr{{Key: "app#5", URL: "https://github.com/o/app/pull/5", Mine: true, Branch: "feat", Checkout: dir}}
	s := loadState()
	for range 2 {
		s = notePrs(s, prs)
		noteEnded("FLD-1", "done")
	}
	evs := loggedEvents(t)
	if got := eventsOf(evs, "pr-opened"); len(got) != 1 || got[0]["key"] != "FLD-1" || got[0]["text"] != "app#5 https://github.com/o/app/pull/5" {
		t.Errorf("pr-opened: %v", got)
	}
	if got := eventsOf(evs, "ticket-done"); len(got) != 1 || got[0]["dispatch"] != s.Dispatched["FLD-1"].ID {
		t.Errorf("ticket-done: %v", got)
	}
}

// The queue and foreman watch both poll: a report either sees, on any number of polls, is logged
// once, and once more when it is rewritten.
func TestReportLoggedOnce(t *testing.T) {
	useFakeRex(t)
	tempState(t)
	dispatched(t, "FLD-1", t.TempDir())
	watch := &watchLoop{note: func(string, Color) {}, lastErr: map[string]string{}, alerted: map[string]string{}}
	pollQueue()
	watch.tick()
	pollQueue()
	watch.tick()
	if got := eventsOf(loggedEvents(t), "reported"); len(got) != 1 || got[0]["text"] != "done" || got[0]["by"] != byAgent {
		t.Fatalf("reported: %v", got)
	}
	report := filepath.Join(evidenceDir("FLD-1"), "report.md")
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(report, []byte("# done, and more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(report, later, later); err != nil {
		t.Fatal(err)
	}
	watch.tick()
	pollQueue()
	evs := loggedEvents(t)
	if len(eventsOf(evs, "reported")) != 1 || len(eventsOf(evs, "report-rewritten")) != 1 {
		t.Errorf("after a rewrite: %v", evs)
	}
}
