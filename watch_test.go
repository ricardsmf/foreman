package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	base := prSignature(watched(t, nil), "me")
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
		if got := prSignature(watched(t, change), "me"); got != base {
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
		if got := prSignature(watched(t, change), "me"); got == base {
			t.Errorf("%s left the signature as it was", what)
		}
	}

	none := watched(t, func(p *watchedPr) { p.Commits.Nodes, p.Comments.Nodes = nil, nil })
	if got := prSignature(none, "me"); got != "head=abc failing= merge=ok comment=none" {
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

// A marked watcher owes a review to a PR that asks for mine and has none of its reviews, and to
// one whose head moved on from the commit it last reviewed, unless I approved since.
func TestDueSignature(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	marked := markedSha("fld-review:sha=")
	bot := func(sha string) myReview {
		return myReview{State: "COMMENTED", Body: "**[bot]** summary\n\n<!-- fld-review:sha=" + sha + " -->"}
	}
	pr := func(asked bool, mine ...myReview) watchedPr {
		p := watchedPr{HeadRefOid: b}
		p.Mine.Nodes = mine
		// A team's request has no login.
		p.ReviewRequests.Nodes = []reviewRequest{{}}
		if asked {
			var r reviewRequest
			r.RequestedReviewer.Login = "Me"
			p.ReviewRequests.Nodes = append(p.ReviewRequests.Nodes, r)
		}
		return p
	}
	for _, c := range []struct {
		what string
		p    watchedPr
		want string
	}{
		{"asked, never reviewed", pr(true), "head=" + b + " since=none"},
		{"asked, reviewed by hand only", pr(true, myReview{State: "COMMENTED", Body: "looks odd"}), "head=" + b + " since=none"},
		{"not asked, never reviewed", pr(false), ""},
		{"not asked, reviewed by hand", pr(false, myReview{State: "CHANGES_REQUESTED", Body: "no"}), ""},
		{"reviewed at the head", pr(true, bot(a), bot(b)), ""},
		{"new commits since its review", pr(false, bot(a)), "head=" + b + " since=" + a},
		{"new commits, and I approved since", pr(false, bot(a), myReview{State: "APPROVED"}), ""},
		{"new commits, approved and then commented", pr(false, bot(a), myReview{State: "APPROVED"}, myReview{State: "COMMENTED", Body: "one more"}), "head=" + b + " since=" + a},
		{"a draft review of mine is no approval yet", pr(false, bot(a), myReview{State: "PENDING"}), "head=" + b + " since=" + a},
		{"a quoted marker before its own", pr(false, myReview{State: "COMMENTED", Body: "quotes <!-- fld-review:sha=" + b + " --> then <!-- fld-review:sha=" + a + " -->"}), "head=" + b + " since=" + a},
		{"a short sha is no marker", pr(false, myReview{State: "COMMENTED", Body: "<!-- fld-review:sha=abc123 -->"}), ""},
	} {
		sig, due := dueSignature(c.p, "me", marked)
		if !due {
			sig = ""
		}
		if sig != c.want {
			t.Errorf("%s: %q, want %q", c.what, sig, c.want)
		}
	}
	// Another watcher's marker isn't this one's.
	if _, due := dueSignature(pr(false, bot(a)), "me", markedSha("other:sha=")); due {
		t.Error("took another marker's review for its own")
	}
}

// The PRs a resting watcher didn't review are kept for the queue, and a wake clears them.
func TestRecordUnreviewed(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })

	recordUnreviewed("w", []string{"org/app#12", "org/app#15"})
	if got := loadState().WatcherUnreviewed["w"]; !slices.Equal(got, []string{"org/app#12", "org/app#15"}) {
		t.Fatalf("recorded %v", got)
	}
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{Name: "w", Dir: "/r/watch", Prompt: "/fld-watch", PRs: []string{"is:open"}, Marker: "m="}}
	label := func(state string) string {
		items := queueItems(loadState(), []rexSession{{ID: "s1", Label: "w", State: state}}, nil)
		_, l := itemLabel(items[0])
		return l
	}
	if l := label("idle"); !strings.Contains(l, "didn't review #12, #15") {
		t.Errorf("at rest: %q", l)
	}
	if l := label("working"); !strings.Contains(l, "working") {
		t.Errorf("working: %q", l)
	}
	recordUnreviewed("w", nil)
	if got, ok := loadState().WatcherUnreviewed["w"]; ok {
		t.Errorf("after a wake: %v", got)
	}
	if l := label("idle"); !strings.Contains(l, "nothing to review") {
		t.Errorf("nothing due: %q", l)
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
		changed, err := wakeIfChanged("w", now, w)
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
	if _, err := wakeIfChanged("w", v3, func() error { return errors.New("no pane") }); err == nil {
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
	if err := wakeWatcher(rexSession{ID: "s1", Block: "b1"}, "/fld-watch"); err != nil {
		t.Fatal(err)
	}
	want := []string{"send s1 b1 /clear", "press s1 b1 enter", "send s1 b1 /fld-watch", "press s1 b1 enter"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %q", f.calls)
	}
	if err := wakeWatcher(rexSession{ID: "s1"}, "/fld-watch"); err == nil {
		t.Error("a watcher with no pane was woken")
	}
}

// A pass's prompt carries the rules for staying inside the allowlist, and a marked watcher's the
// PRs due, in number order, so the pass needn't look them up with commands of its own.
func TestPassPrompt(t *testing.T) {
	w := Watcher{Name: "w", Prompt: "Run a pass."}
	if got := passPrompt(w, map[string]string{"org/app#12": "head=abc failing= merge=ok comment=none"}); got != "Run a pass. "+watcherRules {
		t.Errorf("unmarked: %q", got)
	}
	w.Marker = "m="
	if got := passPrompt(w, nil); got != "Run a pass. "+watcherRules {
		t.Errorf("none due: %q", got)
	}
	got := passPrompt(w, map[string]string{"org/app#120": "head=ccc since=none", "org/app#15": "head=bbb since=aaa"})
	want := "Run a pass. " + watcherRules + " The board already found the PRs due a review, so skip step 1's commands and take these: " +
		"#15 at bbb (new commits since aaa), #120 at ccc (whole PR)."
	if got != want {
		t.Errorf("due:\n got %q\nwant %q", got, want)
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
	err = write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/fld-watch", "marker": "fld-review:sha="}]}`)
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Errorf("a marker without prs: %v", err)
	}
	if err := write(`{"watchers": [{"name": "w", "dir": "/x", "prompt": "/fld-watch", "prs": ["is:open"], "marker": "fld-review:sha="}]}`); err != nil || watchers[0].Marker != "fld-review:sha=" {
		t.Errorf("a marked watcher: %v, %+v", err, watchers)
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
