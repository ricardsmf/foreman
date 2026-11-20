package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A PR as the watch query returns it: head abc, two checks, mergeable, my review and a comment.
const watchedJSON = `{
  "number": 12, "headRefOid": "abc", "mergeable": "MERGEABLE",
  "repository": {"nameWithOwner": "Org/App"},
  "commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
    {"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"},
    {"context": "ci/lint", "state": "PENDING"}
  ]}}}}]},
  "comments": {"nodes": [{"id": "IC_1", "author": {"login": "ana"}, "createdAt": "2026-11-12T10:00:00Z"}]},
  "reviews": {"nodes": [{"id": "PRR_me", "author": {"login": "Me"}, "submittedAt": "2026-11-12T11:00:00Z"}]}
}`

func watched(t *testing.T, change func(*watchedPr)) watchedPr {
	t.Helper()
	var p watchedPr
	if err := json.Unmarshal([]byte(watchedJSON), &p); err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(&p)
	}
	return p
}

func TestPrSignature(t *testing.T) {
	base := prSignature(watched(t, nil), "me", nil)
	if want := "head=abc failing= merge=ok comment=IC_1"; base != want {
		t.Fatalf("signature %q, want %q", base, want)
	}
	if k := watched(t, nil).key(); k != "org/app#12" {
		t.Errorf("key %q", k)
	}
	checks := func(p *watchedPr) *[]prCheck { return &p.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes }
	comment := func(id, login, at string) prComment {
		c := prComment{ID: id, CreatedAt: at}
		c.Author = &struct {
			Login string `json:"login"`
		}{login}
		return c
	}

	same := map[string]func(*watchedPr){
		"GitHub still working out mergeability": func(p *watchedPr) { p.Mergeable = "UNKNOWN" },
		"a queued check starting":               func(p *watchedPr) { (*checks(p))[1].State = "IN_PROGRESS" },
		"a check passing":                       func(p *watchedPr) { (*checks(p))[1].State = "SUCCESS" },
		"a new check queued":                    func(p *watchedPr) { *checks(p) = append(*checks(p), prCheck{Name: "e2e", Status: "QUEUED"}) },
		"a newer comment of mine": func(p *watchedPr) {
			p.Comments.Nodes = append(p.Comments.Nodes, comment("IC_2", "me", "2026-11-12T12:00:00Z"))
		},
		"a pending review, which has no time": func(p *watchedPr) {
			p.Reviews.Nodes = append(p.Reviews.Nodes, prComment{ID: "PRR_draft"})
		},
	}
	for what, change := range same {
		if got := prSignature(watched(t, change), "me", nil); got != base {
			t.Errorf("%s changed the signature: %q", what, got)
		}
	}

	differs := map[string]func(*watchedPr){
		"a new commit":           func(p *watchedPr) { p.HeadRefOid = "def" },
		"a failed check run":     func(p *watchedPr) { (*checks(p))[0].Conclusion = "FAILURE" },
		"a failed commit status": func(p *watchedPr) { (*checks(p))[1].State = "ERROR" },
		"a merge conflict":       func(p *watchedPr) { p.Mergeable = "CONFLICTING" },
		"another's comment": func(p *watchedPr) {
			p.Comments.Nodes = append(p.Comments.Nodes, comment("IC_2", "bob", "2026-11-12T12:00:00Z"))
		},
		"another's review, newer than the comment": func(p *watchedPr) {
			p.Reviews.Nodes = append(p.Reviews.Nodes, prComment{ID: "PRR_bob", SubmittedAt: "2026-11-12T10:30:00Z"})
		},
	}
	for what, change := range differs {
		if got := prSignature(watched(t, change), "me", nil); got == base {
			t.Errorf("%s left the signature as it was", what)
		}
	}

	none := watched(t, func(p *watchedPr) { p.Commits.Nodes, p.Comments.Nodes = nil, nil })
	if got := prSignature(none, "me", nil); got != "head=abc failing= merge=ok comment=none" {
		t.Errorf("no checks or comments: %q", got)
	}

	// A re-run listed beside the failed run, in any order, names the check once.
	rerun := []prCheck{{Name: "test", Conclusion: "FAILURE"}, {Context: "ci/lint", State: "FAILURE"}, {Name: "test", Conclusion: "FAILURE"}, {Name: "e2e", Conclusion: "SUCCESS"}}
	if got := failingChecks(rerun); got != "ci/lint,test" {
		t.Errorf("failing %q", got)
	}
	slices.Reverse(rerun)
	if got := failingChecks(rerun); got != "ci/lint,test" {
		t.Errorf("failing, reversed %q", got)
	}
}

func TestChangedPrs(t *testing.T) {
	last := map[string]string{"a#1": "s1", "a#2": "s2", "a#3": "s3"}
	for _, c := range []struct {
		what string
		now  map[string]string
		want []string
	}{
		{"nothing changed", map[string]string{"a#1": "s1", "a#2": "s2", "a#3": "s3"}, nil},
		{"one changed", map[string]string{"a#1": "s1", "a#2": "s2x", "a#3": "s3"}, []string{"a#2"}},
		{"a new PR", map[string]string{"a#1": "s1", "a#2": "s2", "a#3": "s3", "a#4": "s4"}, []string{"a#4"}},
		{"PRs gone", map[string]string{"a#1": "s1"}, nil},
		{"sorted", map[string]string{"a#3": "x", "a#1": "x"}, []string{"a#1", "a#3"}},
	} {
		if got := changedPrs(last, c.now); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.what, got, c.want)
		}
	}
	if got := changedPrs(nil, map[string]string{"a#1": "s1"}); !slices.Equal(got, []string{"a#1"}) {
		t.Errorf("never woken: %v", got)
	}
}

// The watcher is woken once per change, against the signatures at its last wake, and a failed
// wake is retried on the next poll.
func TestWakeIfChanged(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })

	wakes := 0
	wake := func() error { wakes++; return nil }
	poll := func(now map[string]string, w func() error) []string {
		t.Helper()
		changed, err := wakeIfChanged("w", now, 0, w)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	v1 := map[string]string{"a#1": "s1", "a#2": "s2"}
	if got := poll(v1, wake); !slices.Equal(got, []string{"a#1", "a#2"}) || wakes != 1 {
		t.Fatalf("first poll: %v, %d wakes", got, wakes)
	}
	if got := poll(v1, wake); got != nil || wakes != 1 {
		t.Fatalf("nothing changed: %v, %d wakes", got, wakes)
	}
	// A PR leaving the searches doesn't wake it; one coming back as it was does, since the record
	// follows the last wake, which dropped it.
	v2 := map[string]string{"a#1": "s1"}
	if got := poll(v2, wake); got != nil || wakes != 1 {
		t.Fatalf("a PR gone: %v, %d wakes", got, wakes)
	}
	v3 := map[string]string{"a#1": "s1x"}
	if _, err := wakeIfChanged("w", v3, 0, func() error { return errors.New("no pane") }); err == nil {
		t.Fatal("a failed wake should say so")
	}
	if got := loadState().WatcherPrs["w"]; got["a#1"] != "s1" || got["a#2"] != "s2" {
		t.Fatalf("a failed wake kept the new record: %v", got)
	}
	if got := poll(v3, wake); !slices.Equal(got, []string{"a#1"}) || wakes != 2 {
		t.Fatalf("retry after a failed wake: %v, %d wakes", got, wakes)
	}
	if got := loadState().WatcherPrs["w"]; got["a#1"] != "s1x" || len(got) != 1 {
		t.Errorf("record after the wake: %v", got)
	}
}

// A wake clears the watcher's conversation, then types its prompt.
func TestWakeWatcherClearsFirst(t *testing.T) {
	f := useFakeRex(t)
	w := Watcher{Name: "w", Dir: t.TempDir(), Prompt: "/fld-watch"}
	if err := wakeWatcher(w, rexSession{ID: "s1", Block: "b1"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"send s1 b1 /clear", "press s1 b1 enter", "send s1 b1 " + passPrompt(w), "press s1 b1 enter"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %q", f.calls)
	}
	if err := wakeWatcher(w, rexSession{ID: "s1"}); err == nil {
		t.Error("a watcher with no pane was woken")
	}
}

// A pass's prompt carries the rules for staying inside the allowlist, and names no PR.
func TestPassPrompt(t *testing.T) {
	w := Watcher{Name: "w", Prompt: "Run a pass.", PRs: []string{"is:open"}}
	if got := passPrompt(w); got != "Run a pass. "+watcherRules {
		t.Errorf("prompt %q", got)
	}
}

// A busy watcher, or one not running, isn't typed into: pollWatcher returns before it reads GitHub.
func TestPollWatcherLeavesABusyWatcher(t *testing.T) {
	w := Watcher{Name: "fld-pr-watch", Dir: "/nonexistent/watch", Prompt: "/fld-watch", PRs: []string{"is:open"}}
	for _, state := range []string{"working", "blocked", "open"} {
		changed, err := pollWatcher(w, []rexSession{{ID: "s1", Label: "fld-pr-watch", State: state}}, "s1")
		if changed != nil || err != nil {
			t.Errorf("%s: %v %v", state, changed, err)
		}
	}
	if changed, err := pollWatcher(w, nil, "s1"); changed != nil || err != nil {
		t.Errorf("not running: %v %v", changed, err)
	}
	for state, want := range map[string]bool{"idle": true, "done": true, "error": true, "working": false, "blocked": false, "open": false} {
		if wakeable(state) != want {
			t.Errorf("wakeable(%s) = %v", state, !want)
		}
	}
}

func TestPolledWatcherConfig(t *testing.T) {
	oldFile := configFile
	t.Cleanup(func() { configFile, watchers = oldFile, nil })
	configFile = filepath.Join(t.TempDir(), "config.json")
	write := func(body string) error {
		if err := os.WriteFile(configFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadConfig()
	}
	if err := write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/fld-watch", "prs": ["review-requested:@me"]}]}`); err != nil {
		t.Fatal(err)
	}
	if len(watchers) != 1 || !slices.Equal(watchers[0].PRs, []string{"review-requested:@me"}) {
		t.Errorf("watchers %+v", watchers)
	}
	err := write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/loop 10m /fld-watch", "prs": ["is:open"]}]}`)
	if err == nil || !strings.Contains(err.Error(), "/loop") {
		t.Errorf("a polled watcher on a loop: %v", err)
	}
	if err := write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/loop 10m /fld-watch"}]}`); err != nil {
		t.Errorf("a watcher without prs keeps its loop: %v", err)
	}
	// The marker went with the rules it stood for: a config that still names one is told so.
	err = write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/fld-watch", "prs": ["is:open"], "marker": "fld-review:sha="}]}`)
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Errorf("a marker: %v", err)
	}
}

// A polled watcher between passes waits for a PR to change, not for its next loop.
func TestPolledWatcherLabel(t *testing.T) {
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{Name: "fld-pr-watch", Dir: "/r/watch", Prompt: "/fld-watch", PRs: []string{"is:open"}}}
	items := queueItems(State{}, []rexSession{{ID: "s1", Label: "fld-pr-watch", State: "idle"}}, nil)
	if _, label := itemLabel(items[0]); !strings.Contains(label, "waits for a PR to change") {
		t.Errorf("label %q", label)
	}
	watchers[0].PRs = nil
	items = queueItems(State{}, []rexSession{{ID: "s1", Label: "fld-pr-watch", State: "idle"}}, nil)
	if _, label := itemLabel(items[0]); !strings.Contains(label, "between passes") {
		t.Errorf("label %q", label)
	}
}

// A watcher's `on` picks what wakes it: with commit and threads, a thread opened or resolved does,
// and a failing check or someone's comment doesn't.
func TestPrSignatureParts(t *testing.T) {
	on := []string{"threads", "commit"}
	thread := func(resolved bool) struct {
		IsResolved bool `json:"isResolved"`
	} {
		return struct {
			IsResolved bool `json:"isResolved"`
		}{resolved}
	}
	base := watched(t, func(p *watchedPr) {
		p.ReviewThreads.Nodes = append(p.ReviewThreads.Nodes, thread(true), thread(false))
	})
	if got := prSignature(base, "me", on); got != "head=abc threads=1/2" {
		t.Fatalf("signature %q", got)
	}
	// Without `on`, threads aren't part of it: the signatures recorded before it existed still match.
	if got := prSignature(base, "me", nil); got != "head=abc failing= merge=ok comment=IC_1" {
		t.Errorf("default parts: %q", got)
	}
	change := func(f func(*watchedPr)) string {
		p := base
		p.ReviewThreads.Nodes = slices.Clone(base.ReviewThreads.Nodes)
		f(&p)
		return prSignature(p, "me", on)
	}
	if got := change(func(p *watchedPr) { p.ReviewThreads.Nodes[1].IsResolved = true }); got != "head=abc threads=0/2" {
		t.Errorf("a thread resolved: %q", got)
	}
	if got := change(func(p *watchedPr) { p.ReviewThreads.Nodes = append(p.ReviewThreads.Nodes, thread(false)) }); got != "head=abc threads=2/3" {
		t.Errorf("a thread opened: %q", got)
	}
	if got := change(func(p *watchedPr) { p.HeadRefOid = "def" }); got != "head=def threads=1/2" {
		t.Errorf("a new commit: %q", got)
	}
	// On a PR of its own: the checks are shared with base.
	other := watched(t, func(p *watchedPr) {
		p.ReviewThreads.Nodes = append(p.ReviewThreads.Nodes, thread(true), thread(false))
		p.Mergeable = "CONFLICTING"
		p.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes[0].Conclusion = "FAILURE"
		p.Comments.Nodes = append(p.Comments.Nodes, prComment{ID: "IC_9", CreatedAt: "2026-11-13T10:00:00Z"})
	})
	if same := prSignature(other, "me", on); same != "head=abc threads=1/2" {
		t.Errorf("a conflict, a failed check and a comment changed it: %q", same)
	}
}

// With a batch, a wake counts only that many changed PRs as seen, lowest number first, and the
// rest wake the watcher again: a pass that takes three PRs gets to all of them.
func TestWakeIfChangedInBatches(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })

	wake := func() error { return nil }
	poll := func(now map[string]string) []string {
		t.Helper()
		changed, err := wakeIfChanged("w", now, 2, wake)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	now := map[string]string{"a#9": "s", "a#10": "s", "a#100": "s", "a#11": "s", "a#12": "s"}
	for n, want := range [][]string{{"a#10", "a#9"}, {"a#11", "a#12"}, {"a#100"}, nil} {
		if got := poll(now); !slices.Equal(got, want) {
			t.Fatalf("poll %d: %v, want %v", n+1, got, want)
		}
	}
	// One that changed again while it waited is handed over once, as it is now; a PR gone from the
	// searches leaves the record.
	now = map[string]string{"a#9": "s2", "a#10": "s2", "a#12": "s2"}
	if got := poll(now); !slices.Equal(got, []string{"a#10", "a#9"}) {
		t.Fatalf("after a change: %v", got)
	}
	if got := loadState().WatcherPrs["w"]; !maps.Equal(got, map[string]string{"a#9": "s2", "a#10": "s2", "a#12": "s"}) {
		t.Errorf("record: %v", got)
	}
	// A failed wake leaves the batch unseen too.
	if _, err := wakeIfChanged("w", now, 2, func() error { return errors.New("no pane") }); err == nil {
		t.Fatal("a failed wake should say so")
	}
	if got := poll(now); !slices.Equal(got, []string{"a#12"}) {
		t.Errorf("retry after a failed wake: %v", got)
	}
}

func TestWatcherWakeConfig(t *testing.T) {
	oldFile := configFile
	t.Cleanup(func() { configFile, watchers = oldFile, nil })
	configFile = filepath.Join(t.TempDir(), "config.json")
	write := func(fields string) error {
		body := `{"watchers": [{"name": "w", "dir": "/x", "prompt": "/fld-watch --once", ` + fields + `}]}`
		if err := os.WriteFile(configFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadConfig()
	}
	if err := write(`"prs": ["is:open"], "on": ["commit", "threads"], "batch": 3`); err != nil {
		t.Fatal(err)
	}
	if w := watchers[0]; !slices.Equal(w.On, []string{"commit", "threads"}) || w.Batch != 3 {
		t.Errorf("watcher %+v", w)
	}
	for what, c := range map[string][2]string{
		"an unknown part":      {`"prs": ["is:open"], "on": ["pushes"]`, "pushes"},
		"on without prs":       {`"on": ["commit"]`, "prs"},
		"a batch below nought": {`"prs": ["is:open"], "batch": -1`, "batch"},
	} {
		if err := write(c[0]); err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// writeConversation writes a Claude Code conversation for the watcher in dir, modified at mod, from
// its user entries: what each says, when, and whether Claude Code marks it meta or a subagent's.
func writeConversation(t *testing.T, dir, name string, mod time.Time, entries ...map[string]any) {
	t.Helper()
	conv := claudeProjectDir(dir)
	if err := os.MkdirAll(conv, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b), `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`)
	}
	file := filepath.Join(conv, name+".jsonl")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func userEntry(at time.Time, content any) map[string]any {
	return map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339Nano), "message": map[string]any{"role": "user", "content": content}}
}

// What I type into a watcher counts from its last pass's prompt on, in its newest conversation; the
// pass's own prompt, /clear, a tool's result and what Claude Code or a subagent writes don't.
func TestLastTyped(t *testing.T) {
	oldHome := home
	home = t.TempDir()
	t.Cleanup(func() { home = oldHome })
	dir := "/r/watch"
	if got := lastTyped(dir); !got.IsZero() {
		t.Errorf("no conversation: %v", got)
	}
	t0 := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	clear := userEntry(at(0), "<command-name>/clear</command-name>\n            <command-message>clear</command-message>\n            <command-args></command-args>")
	caveat := userEntry(at(0), "<local-command-caveat>The command below was run directly in Claude Code.</local-command-caveat>")
	caveat["isMeta"] = true
	pass := func(m int) map[string]any {
		return userEntry(at(m), "<command-message>fld-watch</command-message>\n<command-name>/fld-watch</command-name>\n<command-args>--once "+watcherRules+"</command-args>")
	}
	skill := userEntry(at(1), "Base directory for this skill: /r/watch/.claude/skills/fld-watch")
	skill["isMeta"] = true
	toolResult := userEntry(at(2), []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "done"}})
	sidechain := userEntry(at(20), "Review PR 12.")
	sidechain["isSidechain"] = true

	writeConversation(t, dir, "a", at(5), clear, caveat, pass(1), skill, toolResult)
	if got := lastTyped(dir); !got.IsZero() {
		t.Errorf("only the pass: %v", got)
	}
	writeConversation(t, dir, "b", at(30), clear, pass(1), skill, toolResult,
		userEntry(at(10), "draft the three nits as a pending review on 1379"),
		userEntry(at(15), []map[string]any{{"type": "text", "text": "show me the draft"}}),
		sidechain)
	if got := lastTyped(dir); !got.Equal(at(15)) {
		t.Errorf("typed after the pass: %v, want %v", got, at(15))
	}
	writeConversation(t, dir, "c", at(40), userEntry(at(31), "run another watcher pass"), pass(32), toolResult)
	if got := lastTyped(dir); !got.IsZero() {
		t.Errorf("a pass after what I typed: %v", got)
	}
	writeConversation(t, dir, "b", at(50), userEntry(at(45), "what about 1380?"))
	if got := lastTyped(dir); !got.Equal(at(45)) {
		t.Errorf("a watcher started by hand, or resumed: %v", got)
	}
}

// A wake waits talkHold after I last typed into the watcher, by the clock or for a PR: its /clear
// would take that conversation. pollWatcher returns before it reads GitHub.
func TestWakeWaitsWhileITalk(t *testing.T) {
	f := useFakeRex(t)
	oldHome := home
	home = t.TempDir()
	t.Cleanup(func() { home = oldHome })
	w := Watcher{Name: "eod", Dir: "/r/eod", Prompt: "Write my update.", At: "11:59", PRs: []string{"is:open"}}
	sessions := []rexSession{{ID: "s1", Label: "eod", State: "idle", Block: "b1"}}
	typed := time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local)
	writeConversation(t, w.Dir, "a", typed, userEntry(typed, "post the update, but drop the second line"))
	if _, err := updateState(func(s *State) { s.WatcherRan["eod"] = typed.Add(-48 * time.Hour).Format(time.RFC3339) }); err != nil {
		t.Fatal(err)
	}
	if at, err := pollSchedule(w, sessions, "s1", typed.Add(talkHold-time.Minute)); !at.IsZero() || err != nil {
		t.Errorf("by the clock, while I talk: %v %v", at, err)
	}
	now := time.Now()
	writeConversation(t, w.Dir, "b", now, userEntry(now.Add(-10*time.Minute), "is 1380 ready?"))
	if changed, err := pollWatcher(w, sessions, "s1"); changed != nil || err != nil {
		t.Errorf("for a PR, while I talk: %v %v", changed, err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("woke a watcher I'm talking to: %q", f.calls)
	}
	writeConversation(t, w.Dir, "c", now.Add(time.Second), userEntry(typed, "post the update, but drop the second line"))
	if at, err := pollSchedule(w, sessions, "s1", typed.Add(talkHold+time.Minute)); at.IsZero() || err != nil {
		t.Errorf("by the clock, once I stopped: %v %v", at, err)
	}
	if len(f.calls) == 0 || f.calls[0] != "send s1 b1 /clear" {
		t.Errorf("calls %q", f.calls)
	}
}
