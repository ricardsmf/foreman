package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// threadsQueue is conflictQueue's PR without the conflict and with two review threads to answer,
// and a gh on PATH that gives its discussion: one thread open, one resolved.
func threadsQueue(t *testing.T) (q *queueModel, rex *fakeRex, onBranch string, p Pr) {
	t.Helper()
	q, rex, onBranch, p = conflictQueue(t)
	p.Conflict, p.Threads, p.Status = false, 2, "2 threads ✓"
	q.prs[0] = p
	bin := t.TempDir()
	gh := "#!/bin/sh\ncat <<'EOF'\n" + `{"data":{"repository":{"pullRequest":{
  "reviews":{"totalCount":0,"nodes":[]},
  "comments":{"totalCount":0,"nodes":[]},
  "reviewThreads":{"totalCount":2,"nodes":[
    {"isResolved":false,"isOutdated":false,"path":"src/a.go","line":12,"originalLine":12,
     "comments":{"totalCount":1,"nodes":[{"author":{"login":"ana"},"body":"Name this better.","createdAt":"2026-11-25"}]}},
    {"isResolved":true,"isOutdated":false,"path":"src/b.go","line":3,"originalLine":3,
     "comments":{"totalCount":1,"nodes":[{"author":{"login":"ana"},"body":"Done already.","createdAt":"2026-11-25"}]}}]}}}}}` + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return q, rex, onBranch, p
}

// pressT puts the cursor on the PR's row and presses t.
func pressT(t *testing.T, q *queueModel, sessions ...rexSession) tea.Cmd {
	t.Helper()
	q.state, q.sessions = loadState(), sessions
	q.rebuild()
	q.cursor = slices.IndexFunc(q.items, func(it queueItem) bool { return it.Kind == prItem })
	if q.cursor < 0 {
		t.Fatal("the queue lists no PR")
	}
	_, cmd := q.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	return cmd
}

// t on a PR with threads to answer writes them beside the report of the agent on its branch and
// tells it to address them, in its pane; a closed agent is resumed on it.
func TestThreadsGoToTheAgentOnTheBranch(t *testing.T) {
	q, rex, dir, p := threadsQueue(t)
	dispatched(t, "FLD-1", dir)
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session, d.Agent = "s1", agentClaude }) })
	file := threadsFile("FLD-1")
	ask := threadsAsk(p, file)
	for _, want := range []string{"PR #7, on this branch, has review threads to answer", file, "Address each unresolved thread", "Don't push, reply to or resolve"} {
		if !strings.Contains(ask, want) {
			t.Errorf("the ask lacks %q: %s", want, ask)
		}
	}
	if strings.Contains(ask, "\n") {
		t.Errorf("the ask has a newline, which a pane takes as enter: %q", ask)
	}

	send := pressT(t, q, rexSession{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"})
	if send == nil {
		t.Fatalf("t sent nothing: %q", q.status)
	}
	if said := deliverNow(t, send); said != "✓ Told FLD-1 to address #7's review threads" {
		t.Errorf("said %q", said)
	}
	if len(rex.calls) != 2 || rex.calls[0] != "send s1 b1 "+ask || rex.calls[1] != "press s1 b1 enter" {
		t.Errorf("typed into the pane: %q", rex.calls)
	}
	written, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Review threads on me/app#7", "### `src/a.go:12`", "> Name this better.", "1 resolved threads left out"} {
		if !strings.Contains(string(written), want) {
			t.Errorf("%s lacks %q:\n%s", file, want, written)
		}
	}
	if strings.Contains(string(written), "Done already.") {
		t.Errorf("%s shows a resolved thread:\n%s", file, written)
	}
	if log := logLines(readLog("FLD-1")); len(log) != 3 || !strings.HasSuffix(log[2], "**me sends**: address #7's review threads") {
		t.Errorf("the log: %q", log)
	}
	if keys := q.rowKeys(q.items[q.cursor]); keys[0] != "t fix the threads" {
		t.Errorf("the row's keys start with %q", keys[0])
	}

	// A prompt would take what is typed as its answer.
	rex.calls = nil
	if cmd := pressT(t, q, rexSession{ID: "s1", Label: "fld-1", State: "blocked", Block: "b1"}); cmd != nil || len(rex.calls) != 0 || !strings.Contains(q.status, "FLD-1 waits on a prompt; answer it first, then t again") {
		t.Errorf("a blocked agent: typed %q, status %q", rex.calls, q.status)
	}

	// Its session closed: it resumes on the ask.
	resume := pressT(t, q)
	if resume == nil {
		t.Fatalf("t resumed nothing: %q", q.status)
	}
	if said := deliverNow(t, resume); said != "✓ Resumed FLD-1 to address #7's review threads" {
		t.Errorf("said %q", said)
	}
	cmd := rex.cmds["fld-1-fld-1"]
	if len(cmd) == 0 || cmd[len(cmd)-1] != ask || !slices.Contains(cmd, "--continue") {
		t.Errorf("resumed with %q; want the conversation continued on the ask", cmd)
	}
}

// The agent fixing the PR comes before its ticket's.
func TestThreadsGoToTheFixAgentFirst(t *testing.T) {
	q, rex, dir, _ := threadsQueue(t)
	dispatched(t, "FLD-1", dir)
	dispatched(t, "fix:app#7", dir)
	_, _ = updateState(func(s *State) {
		s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" })
		s.change("fix:app#7", func(d *Dispatched) { d.Session = "s2" })
	})
	send := pressT(t, q, rexSession{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"}, rexSession{ID: "s2", Label: "fix-app-7", State: "idle", Block: "b2"})
	if send == nil {
		t.Fatalf("t sent nothing: %q", q.status)
	}
	if said := deliverNow(t, send); said != "✓ Told fix:app#7 to address #7's review threads" || !strings.HasPrefix(rex.calls[0], "send s2 b2 PR #7") {
		t.Errorf("said %q, typed %q", said, rex.calls)
	}
	if _, err := os.Stat(threadsFile("fix:app#7")); err != nil {
		t.Errorf("the threads aren't beside the fix's report: %v", err)
	}
}

// With no agent on the PR's branch, t opens the board's form on the PR as a fix, whose prompt
// carries the threads: an agent the ticket has on another branch isn't told.
func TestThreadsWithoutAnAgentOpenTheFixForm(t *testing.T) {
	q, rex, _, _ := threadsQueue(t)
	other := filepath.Join(t.TempDir(), "app-fld-1-docs")
	git(t, filepath.Dir(other), "init", "-q", "-b", "rf/fld-1-docs", other)
	git(t, other, "commit", "-q", "--allow-empty", "-m", "base")
	dispatched(t, "FLD-1", other)
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" }) })
	cmd := pressT(t, q, rexSession{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"})
	if cmd == nil {
		t.Fatalf("t did nothing: %q", q.status)
	}
	msg, ok := cmd().(openFormMsg)
	if !ok || msg.pr == nil || msg.pr.Key != "app#7" || msg.context != "" || len(rex.calls) != 0 {
		t.Fatalf("got %+v, typed %q; want the form asked for on app#7", msg, rex.calls)
	}
	a := testApp(t, false)
	a.focus = queueSide
	a.Update(msg)
	if f := a.board.form; a.focus != boardSide || f == nil || f.row.Key != "app#7" || !f.fix || f.context != "" {
		t.Fatalf("the board: focus %d, form %+v; want a fix of app#7", a.focus, f)
	}
}

// t is for a PR with review threads to answer.
func TestThreadsKeyElsewhere(t *testing.T) {
	q, rex, dir, _ := threadsQueue(t)
	q.prs[0].Threads = 0
	dispatched(t, "FLD-1", dir)
	if cmd := pressT(t, q); cmd != nil || len(rex.calls) != 0 || !strings.Contains(q.status, "app#7 isn't one") {
		t.Errorf("a PR without threads: typed %q, status %q", rex.calls, q.status)
	}
	q.cursor = slices.IndexFunc(q.items, func(it queueItem) bool { return it.Kind == agentItem && it.Key == "FLD-1" })
	if _, cmd := q.Update(tea.KeyPressMsg{Code: 't', Text: "t"}); cmd != nil || !strings.Contains(q.status, "FLD-1 isn't one") {
		t.Errorf("an agent's row: status %q", q.status)
	}
}
