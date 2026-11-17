package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
		lastErr: map[string]string{}, alerted: map[string]string{}, behind: map[string]string{}, checked: map[string]time.Time{},
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
	now := time.Now()
	running := []rexSession{{ID: "session:looper", Label: "looper", State: "idle"}}
	starts := func() int { return count(f.calls, "new looper") }

	f.down = 2
	if wait := l.tick(now); wait != rexWait || starts() != 0 {
		t.Fatalf("Rex down: wait %v, %d starts", wait, starts())
	}
	l.tick(now)
	if got := count(*notes, "Waiting for Rex"); got != 1 {
		t.Errorf("said it waits for Rex %d times: %q", got, *notes)
	}
	if wait := l.tick(now); wait != watchPoll || starts() != 1 || count(*notes, "Started looper") != 1 {
		t.Fatalf("Rex up: wait %v, %d starts, %q", wait, starts(), *notes)
	}
	f.open = running
	if l.tick(now); starts() != 1 {
		t.Fatalf("started a running watcher again: %q", f.calls)
	}
	f.open = nil
	if l.tick(now); starts() != 2 {
		t.Fatalf("a watcher whose session is gone: %d starts", starts())
	}
	if _, err := updateState(func(s *State) { s.WatcherStopped["looper"] = true }); err != nil {
		t.Fatal(err)
	}
	if l.tick(now); starts() != 2 {
		t.Fatalf("started a watcher I stopped: %q", f.calls)
	}
	f.open = running
	if l.tick(now); loadState().WatcherStopped["looper"] {
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

// A watcher that waits on me, or failed, is told once per thing it waits on.
func TestTickAlerts(t *testing.T) {
	f := useFakeRex(t)
	l, _, told := testLoop(t)
	watchers = []Watcher{{Name: "looper", Dir: t.TempDir(), Prompt: "/loop 10m /sweep"}}
	now := time.Now()
	in := func(state, kind, msg string) {
		f.open = []rexSession{{ID: "s1", Label: "looper", Panes: []rexPane{pane("block:1", true, rexRecord{State: state, Kind: kind, Msg: msg})}}}
		l.tick(now)
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

// A watcher whose checkout is behind its repo in .claude is told once per upstream commit; commits
// elsewhere in the repo say nothing.
func TestSkillsBehind(t *testing.T) {
	useFakeRex(t)
	l, _, told := testLoop(t)
	root := t.TempDir()
	origin, dir := filepath.Join(root, "origin"), filepath.Join(root, "watch")
	git(t, root, "init", "-q", "-b", "develop", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, root, "clone", "-q", origin, dir)
	git(t, dir, "checkout", "-q", "--detach", "origin/develop")
	w := Watcher{Name: "w", Dir: dir, Prompt: "/sweep"}
	commit := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(origin, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(origin, path), "x")
		git(t, origin, "add", "-A")
		git(t, origin, "commit", "-q", "-m", "change "+path)
	}
	now := time.Now()
	check := func(what string, want int) {
		t.Helper()
		now = now.Add(2 * skillsCheck)
		l.skills(w, now)
		if len(*told) != want {
			t.Fatalf("%s: told %d times: %q", what, len(*told), *told)
		}
	}
	check("up to date", 0)
	commit("src/app.ts")
	check("a commit outside .claude", 0)
	commit(".claude/skills/sweep/SKILL.md")
	check("a skill changed", 1)
	if want := "w runs old skills: 1 commits on origin/develop changed .claude. Move its checkout forward."; (*told)[0] != want {
		t.Errorf("told %q", (*told)[0])
	}
	check("the same commit again", 1)
	// Within the hour it doesn't look at all.
	commit(".claude/agents/reviewer.md")
	if l.skills(w, now.Add(time.Minute)); len(*told) != 1 {
		t.Errorf("looked again within the hour: %q", *told)
	}
	check("another skill commit", 2)
	git(t, dir, "checkout", "-q", "--detach", "origin/develop")
	check("moved forward", 2)
	// A directory that is no checkout says nothing.
	l.skills(Watcher{Name: "eod", Dir: t.TempDir()}, now)
	if len(*told) != 2 {
		t.Errorf("a plain directory: %q", *told)
	}
}
