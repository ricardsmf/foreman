package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// conflictQueue is a queue holding one PR of mine, app#7, which conflicts with develop, and a
// checkout on its branch for an agent to be dispatched in.
func conflictQueue(t *testing.T) (q *queueModel, rex *fakeRex, onBranch string, p Pr) {
	t.Helper()
	rex = useFakeRex(t)
	tempState(t)
	onBranch = filepath.Join(t.TempDir(), "app-fld-1")
	git(t, filepath.Dir(onBranch), "init", "-q", "-b", "rf/fld-1-add", onBranch)
	git(t, onBranch, "commit", "-q", "--allow-empty", "-m", "base")
	p = Pr{Key: "app#7", Summary: "FLD-1 Add a thing", Repo: "me/app", Number: 7, Branch: "rf/fld-1-add", Base: "develop",
		Checkout: onBranch, Mine: true, Conflict: true, Status: "conflict ✓", Category: "Blocked"}
	q = &queueModel{details: map[string][]string{}, width: 100, height: 30, prs: []Pr{p}}
	return q, rex, onBranch, p
}

// pressM puts the cursor on the PR's row and presses m.
func pressM(t *testing.T, q *queueModel, sessions ...rexSession) tea.Cmd {
	t.Helper()
	q.state, q.sessions = loadState(), sessions
	q.rebuild()
	q.cursor = slices.IndexFunc(q.items, func(it queueItem) bool { return it.Kind == prItem })
	if q.cursor < 0 {
		t.Fatal("the queue lists no PR")
	}
	_, cmd := q.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	return cmd
}

// m on a PR that conflicts tells the agent on its branch to merge the base in, in its pane; a
// closed agent is resumed on it.
func TestConflictGoesToTheAgentOnTheBranch(t *testing.T) {
	q, rex, dir, p := conflictQueue(t)
	dispatched(t, "FLD-1", dir)
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session, d.Agent = "s1", agentClaude }) })
	ask := conflictAsk(p)
	for _, want := range []string{"PR #7, on this branch, conflicts with its base, `develop`", "merge `origin/develop` into the branch", "not a rebase", "Don't push."} {
		if !strings.Contains(ask, want) {
			t.Errorf("the ask lacks %q: %s", want, ask)
		}
	}
	if strings.Contains(ask, "\n") {
		t.Errorf("the ask has a newline, which a pane takes as enter: %q", ask)
	}

	send := pressM(t, q, rexSession{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"})
	if send == nil {
		t.Fatalf("m sent nothing: %q", q.status)
	}
	if said := deliverNow(t, send); said != "✓ Told FLD-1 to merge develop into #7" {
		t.Errorf("said %q", said)
	}
	if len(rex.calls) != 2 || rex.calls[0] != "send s1 b1 "+ask || rex.calls[1] != "press s1 b1 enter" {
		t.Errorf("typed into the pane: %q", rex.calls)
	}
	if log := logLines(readLog("FLD-1")); len(log) != 3 || !strings.HasSuffix(log[2], "**me sends**: merge develop into #7") {
		t.Errorf("the log: %q", log)
	}
	if keys := q.rowKeys(q.items[q.cursor]); keys[0] != "m merge develop in" {
		t.Errorf("the row's keys start with %q", keys[0])
	}

	// A prompt would take what is typed as its answer.
	rex.calls = nil
	if cmd := pressM(t, q, rexSession{ID: "s1", Label: "fld-1", State: "blocked", Block: "b1"}); cmd != nil || len(rex.calls) != 0 || !strings.Contains(q.status, "FLD-1 waits on a prompt; answer it first") {
		t.Errorf("a blocked agent: typed %q, status %q", rex.calls, q.status)
	}

	// Its session closed: it resumes on the ask.
	resume := pressM(t, q)
	if resume == nil {
		t.Fatalf("m resumed nothing: %q", q.status)
	}
	if said := deliverNow(t, resume); said != "✓ Resumed FLD-1 to merge develop into #7" {
		t.Errorf("said %q", said)
	}
	cmd := rex.cmds["fld-1-fld-1"]
	if len(cmd) == 0 || cmd[len(cmd)-1] != ask || !slices.Contains(cmd, "--continue") {
		t.Errorf("resumed with %q; want the conversation continued on the ask", cmd)
	}
}

// The agent fixing the PR comes before its ticket's.
func TestConflictGoesToTheFixAgentFirst(t *testing.T) {
	q, rex, dir, _ := conflictQueue(t)
	dispatched(t, "FLD-1", dir)
	dispatched(t, "fix:app#7", dir)
	_, _ = updateState(func(s *State) {
		s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" })
		s.change("fix:app#7", func(d *Dispatched) { d.Session = "s2" })
	})
	send := pressM(t, q, rexSession{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"}, rexSession{ID: "s2", Label: "fix-app-7", State: "idle", Block: "b2"})
	if send == nil {
		t.Fatalf("m sent nothing: %q", q.status)
	}
	if said := deliverNow(t, send); said != "✓ Told fix:app#7 to merge develop into #7" || !strings.HasPrefix(rex.calls[0], "send s2 b2 PR #7") {
		t.Errorf("said %q, typed %q", said, rex.calls)
	}
}

// With no agent on the PR's branch, m opens the board's form on the PR, as a fix that asks for
// the merge: an agent the ticket has on another branch isn't told.
func TestConflictWithoutAnAgentOpensTheFixForm(t *testing.T) {
	q, rex, _, p := conflictQueue(t)
	other := filepath.Join(t.TempDir(), "app-fld-1-docs")
	git(t, filepath.Dir(other), "init", "-q", "-b", "rf/fld-1-docs", other)
	git(t, other, "commit", "-q", "--allow-empty", "-m", "base")
	dispatched(t, "FLD-1", other)
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" }) })
	cmd := pressM(t, q, rexSession{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"})
	if cmd == nil {
		t.Fatalf("m did nothing: %q", q.status)
	}
	msg, ok := cmd().(openFormMsg)
	if !ok || msg.pr == nil || msg.pr.Key != "app#7" || msg.context != conflictAsk(p) || len(rex.calls) != 0 {
		t.Fatalf("got %+v, typed %q; want the form asked for on app#7", msg, rex.calls)
	}

	a := testApp(t, false)
	a.focus = queueSide
	a.Update(msg)
	f := a.board.form
	if a.focus != boardSide || f == nil || f.row.Key != "app#7" || !f.fix || f.context != conflictAsk(p) {
		t.Fatalf("the board: focus %d, form %+v; want a fix of app#7 with the ask", a.focus, f)
	}
	// A dispatch under way keeps its form.
	f.busy, f.context = true, "mine"
	a.Update(msg)
	if a.board.form != f || f.context != "mine" || !strings.Contains(a.board.status, "press m on it again") {
		t.Errorf("a busy form: context %q, status %q", a.board.form.context, a.board.status)
	}
}

// m is for a PR that conflicts.
func TestConflictKeyElsewhere(t *testing.T) {
	q, rex, dir, _ := conflictQueue(t)
	q.prs[0].Conflict = false
	dispatched(t, "FLD-1", dir)
	if cmd := pressM(t, q); cmd != nil || len(rex.calls) != 0 || !strings.Contains(q.status, "app#7 isn't one") {
		t.Errorf("a PR that merges cleanly: typed %q, status %q", rex.calls, q.status)
	}
	q.cursor = slices.IndexFunc(q.items, func(it queueItem) bool { return it.Kind == agentItem && it.Key == "FLD-1" })
	if _, cmd := q.Update(tea.KeyPressMsg{Code: 'm', Text: "m"}); cmd != nil || !strings.Contains(q.status, "FLD-1 isn't one") {
		t.Errorf("an agent's row: status %q", q.status)
	}
}
