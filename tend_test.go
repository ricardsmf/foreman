package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A watchLoop for a test: its notes and its notifications are kept, and Rex is asked at once.
func testLoop(t *testing.T) (l *watchLoop, notes, told *[]string) {
	t.Helper()
	notes, told = &[]string{}, &[]string{}
	oldNotify, oldWait := notify, rexWait
	notify = func(_, text string) error { *told = append(*told, text); return nil }
	rexWait = 0
	t.Cleanup(func() { notify, rexWait, watchers = oldNotify, oldWait, nil })
	return &watchLoop{
		note:    func(text string, _ Color) { *notes = append(*notes, text) },
		lastErr: map[string]string{}, alerted: map[string]string{},
	}, notes, told
}

func count(lines []string, part string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, part) {
			n++
		}
	}
	return n
}

// foreman watch waits for Rex, saying so once, then starts the watchers. One whose session is gone
// is started again, unless I stopped it; a session that turns up under its name takes the stop back.
func TestTickStartsWatchers(t *testing.T) {
	f := useFakeRex(t)
	l, notes, _ := testLoop(t)
	watchers = []Watcher{{Name: "looper", Dir: t.TempDir(), Prompt: "/loop 10m /sweep"}}
	running := []rexSession{{ID: "session:looper", Label: "looper", State: "idle"}}
	starts := func() int { return count(f.calls, "new looper") }

	f.down = 2
	if wait := l.tick(); wait != rexWait || starts() != 0 {
		t.Fatalf("Rex down: wait %v, %d starts", wait, starts())
	}
	l.tick()
	if got := count(*notes, "Waiting for Rex"); got != 1 {
		t.Errorf("said it waits for Rex %d times: %q", got, *notes)
	}
	if wait := l.tick(); wait != watchPoll || starts() != 1 || count(*notes, "Started looper") != 1 {
		t.Fatalf("Rex up: wait %v, %d starts, %q", wait, starts(), *notes)
	}
	f.open = running
	if l.tick(); starts() != 1 {
		t.Fatalf("started a running watcher again: %q", f.calls)
	}
	f.open = nil
	if l.tick(); starts() != 2 {
		t.Fatalf("a watcher whose session is gone: %d starts", starts())
	}
	if _, err := updateState(func(s *State) { s.WatcherStopped["looper"] = true }); err != nil {
		t.Fatal(err)
	}
	if l.tick(); starts() != 2 {
		t.Fatalf("started a watcher I stopped: %q", f.calls)
	}
	f.open = running
	if l.tick(); loadState().WatcherStopped["looper"] {
		t.Error("a watcher running again is still marked stopped")
	}
}

// Closing a watcher's session from the queue stops it for every foreman, until the queue starts it
// again; closing an agent's marks nothing.
func TestStoppedWatcherStaysStopped(t *testing.T) {
	f := useFakeRex(t)
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{Name: "looper", Dir: t.TempDir(), Prompt: "/loop 10m /sweep"}}
	starts := func() int { return count(f.calls, "new looper") }

	if err := closeSession("s1", "looper", true); err != nil || !slices.Contains(f.calls, "kill s1") {
		t.Fatalf("closing it: %v, %q", err, f.calls)
	}
	if started, err := startWatchers(); err != nil || len(started) != 0 || starts() != 0 {
		t.Fatalf("a stopped watcher was started: %v %v", started, err)
	}
	if err := restartWatcher("looper"); err != nil || starts() != 1 || loadState().WatcherStopped["looper"] {
		t.Fatalf("starting it from the queue: %v, %d starts", err, starts())
	}
	if err := closeSession("s2", "FLD-1", false); err != nil || len(loadState().WatcherStopped) != 0 {
		t.Errorf("an agent's session: %v, stopped %v", err, loadState().WatcherStopped)
	}
}

// One foreman watch at a time: a second fails to start while the first holds the lock, and the
// queue's row of a watcher it would wake says when none runs.
func TestOneWatchAtATime(t *testing.T) {
	tempState(t)
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{Name: "w", Dir: "/r/watch", Prompt: "/fld-watch", PRs: []string{"is:open"}}, {Name: "looper", Dir: "/r/loop", Prompt: "/loop 10m /sweep"}}
	if watchRuns() {
		t.Fatal("a foreman watch runs before any started")
	}
	release, err := holdWatchLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holdWatchLock(); err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Errorf("a second foreman watch: %v", err)
	}
	if !watchRuns() {
		t.Error("the lock is held and no foreman watch is seen")
	}
	release()
	if watchRuns() {
		t.Error("a foreman watch is seen after it stopped")
	}

	q := &queueModel{details: map[string][]string{}}
	sessions := []rexSession{{ID: "s1", Label: "w", State: "idle"}, {ID: "s2", Label: "looper", State: "idle"}}
	labels := func(watching bool) map[string]string {
		q.Update(queueStateMsg{State{}, sessions, true, watching})
		out := map[string]string{}
		for _, it := range q.all {
			_, out[it.Key] = itemLabel(it)
		}
		return out
	}
	if got := labels(false); !strings.Contains(got["w"], "foreman watch isn't running") || !strings.Contains(got["looper"], "between passes") {
		t.Errorf("no foreman watch: %q", got)
	}
	if got := labels(true); !strings.Contains(got["w"], "waits for a PR to change") {
		t.Errorf("a foreman watch runs: %q", got)
	}
}

// A watcher that waits on me, or failed, is told once per thing it waits on.
func TestTickAlerts(t *testing.T) {
	f := useFakeRex(t)
	l, _, told := testLoop(t)
	watchers = []Watcher{{Name: "looper", Dir: t.TempDir(), Prompt: "/loop 10m /sweep"}}
	in := func(state, kind, msg string) {
		f.open = []rexSession{{ID: "s1", Label: "looper", Panes: []rexPane{pane("block:1", true, rexRecord{State: state, Kind: kind, Msg: msg})}}}
		l.tick()
	}
	in("working", "", "Reviewing #12")
	if len(*told) != 0 {
		t.Fatalf("told of a working watcher: %q", *told)
	}
	in("blocked", "permission", "Bash: gh pr merge 12")
	in("blocked", "permission", "Bash: gh pr merge 12")
	if want := []string{"looper waits on you: permission Bash: gh pr merge 12"}; !slices.Equal(*told, want) {
		t.Fatalf("a prompt: %q", *told)
	}
	in("blocked", "", "Bash: rm -rf x")
	if len(*told) != 2 || (*told)[1] != "looper waits on you: permission Bash: rm -rf x" {
		t.Fatalf("the next prompt: %q", *told)
	}
	in("idle", "", "")
	in("error", "", "")
	if len(*told) != 3 || (*told)[2] != "looper failed" {
		t.Fatalf("a failure: %q", *told)
	}
}

// A dispatched agent that waits on a prompt, or failed, is told the same way.
func TestTickAlertsOfDispatchedAgents(t *testing.T) {
	f := useFakeRex(t)
	l, _, told := testLoop(t)
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"], s.Panes["s1"] = Dispatched{Dir: "/w", Session: "s1"}, "block:1"
		s.Dispatched["FLD-2"] = Dispatched{Dir: "/w2", Session: "closed"}
	}); err != nil {
		t.Fatal(err)
	}
	in := func(state, kind, msg string) {
		f.open = []rexSession{{ID: "s1", Label: "fld-1-x", Panes: []rexPane{pane("block:1", true, rexRecord{State: state, Kind: kind, Msg: msg})}}}
		l.tick()
	}
	in("working", "", "Running the tests")
	in("blocked", "permission", "Bash: git push")
	in("blocked", "permission", "Bash: git push")
	in("working", "", "")
	in("error", "", "")
	if want := []string{"FLD-1 waits on you: permission Bash: git push", "FLD-1 failed"}; !slices.Equal(*told, want) {
		t.Errorf("told %q, want %q", *told, want)
	}
}

// A wake moves the watcher's checkout to the tip of origin/develop before its pass, fetching
// first; a checkout on a branch stays where it is, and one that can't move holds the pass back.
func TestWakeMovesCheckoutForward(t *testing.T) {
	f := useFakeRex(t)
	root := t.TempDir()
	origin, dir := filepath.Join(root, "origin"), filepath.Join(root, "watch")
	git(t, root, "init", "-q", "-b", "develop", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, root, "clone", "-q", origin, dir)
	git(t, dir, "checkout", "-q", "--detach", "origin/develop")
	w := Watcher{Name: "w", Dir: dir, Prompt: "/sweep"}
	s := rexSession{ID: "s1", Block: "b1"}
	n := 0
	commit := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(origin, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		n++
		writeFile(t, filepath.Join(origin, path), strconv.Itoa(n))
		git(t, origin, "add", "-A")
		git(t, origin, "commit", "-q", "-m", "change "+path)
		return git(t, origin, "rev-parse", "HEAD")
	}
	head := func() string { return git(t, dir, "rev-parse", "HEAD") }

	tip := commit(".claude/skills/sweep/SKILL.md")
	if err := wakeWatcher(w, s); err != nil {
		t.Fatal(err)
	}
	if head() != tip {
		t.Fatalf("the wake left the checkout at %s, not origin's %s", head(), tip)
	}
	if !slices.Contains(f.calls, "send s1 b1 "+passPrompt(w)) {
		t.Errorf("no pass after the move: %q", f.calls)
	}

	// A file the pass left behind, untracked, doesn't stop the move.
	writeFile(t, filepath.Join(dir, "notes.txt"), "x")
	tip = commit("src/app.ts")
	if err := wakeWatcher(w, s); err != nil || head() != tip {
		t.Fatalf("with an untracked file: %v, at %s", err, head())
	}

	// A change to a file the next commit changes too stops it, and the pass with it.
	writeFile(t, filepath.Join(dir, "src/app.ts"), "mine")
	commit("src/app.ts")
	f.calls = nil
	if err := wakeWatcher(w, s); err == nil || !strings.Contains(err.Error(), "moving its checkout forward") || len(f.calls) != 0 {
		t.Fatalf("a checkout that can't move: %v, calls %q", err, f.calls)
	}
	git(t, dir, "checkout", "-q", "--", "src/app.ts")

	// A checkout on a branch is someone's work.
	git(t, dir, "checkout", "-q", "-b", "mine")
	before := head()
	commit(".claude/agents/reviewer.md")
	if err := wakeWatcher(w, s); err != nil || head() != before {
		t.Fatalf("on a branch: %v, moved to %s", err, head())
	}

	// A directory that is no checkout has nothing to move.
	if err := moveForward(t.TempDir()); err != nil {
		t.Errorf("a plain directory: %v", err)
	}
}
