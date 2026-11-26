package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// foreman oracle records each run, and the queue counts the oracles as they stand: how many
// are green, and which are green without ever having been red.
func TestOracleCountsAndFlagsNeverRed(t *testing.T) {
	useFakeRex(t)
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	record := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runOracle(append([]string{"--key", "FLD-1"}, args...), &out); err != nil {
			t.Fatalf("foreman oracle %v: %v", args, err)
		}
		return out.String()
	}
	if got := record("login test", "red", "expected", "200,", "got 500"); got != "FLD-1: 0/1 green\n" {
		t.Errorf("a red run printed %q", got)
	}
	if got := record("Login Test", "GREEN", "passes"); got != "FLD-1: 1/1 green\n" {
		t.Errorf("the same oracle green, whatever its case, printed %q", got)
	}
	if got := record("written to fit", "green"); !strings.HasPrefix(got, "FLD-1: 2/2 green, 1 never red\n") || !strings.Contains(got, `"written to fit" is green and was never recorded red`) {
		t.Errorf("a green run never red printed %q", got)
	}
	record("--stays-green", "lint", "green")
	record("still failing", "red", "times out")
	oracles := readOracles("FLD-1")
	if len(oracles) != 4 || oracleCount(oracles) != "3/4 green, 1 never red" {
		t.Fatalf("the oracles: %+v, %q", oracles, oracleCount(oracles))
	}
	if o := oracles[0]; o.Name != "login test" || o.Result != green || !o.WasRed || o.Note != "passes" || o.neverRed() {
		t.Errorf("the first: %+v", o)
	}
	if !oracles[1].neverRed() || oracles[2].neverRed() || oracles[3].neverRed() {
		t.Errorf("never red: %t %t %t; want only the one written to fit", oracles[1].neverRed(), oracles[2].neverRed(), oracles[3].neverRed())
	}
	for _, bad := range [][]string{{"name"}, {"name", "yellow"}, {"--stays-green", "name", "red"}, {" ", "green"}} {
		if err := runOracle(append([]string{"--key", "FLD-1"}, bad...), &bytes.Buffer{}); err == nil {
			t.Errorf("foreman oracle %q: want an error", bad)
		}
	}

	// The queue's row, its detail and foreman agents carry them.
	q := &queueModel{details: map[string][]string{}, width: 120, height: 30, state: loadState()}
	q.rebuild()
	if screen := ansi.Strip(q.content()); !strings.Contains(screen, "3/4 green, 1 never red") {
		t.Errorf("the row lacks the count:\n%s", screen)
	}
	detail := strings.Join(itemDetail(q.all[0]), "\n")
	for _, want := range []string{"## Oracles", "- ✓ `login test` green, was red: passes", "- ✓ `written to fit` green, **never red**", "- ✓ `lint` green, stays green", "- ✗ `still failing` **red**: times out"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail lacks %q:\n%s", want, detail)
		}
	}
	if info := agentInfoOf(q.all[0], q.state); len(info.Oracles) != 4 || info.Oracles[1].WasRed {
		t.Errorf("agents --json: %+v", info.Oracles)
	}
	// Every oracle green and seen red reads green; any other count stands out.
	if mark := oracleMark(oracles[:1]); !strings.Contains(looks(mark), "fg=ansi2") {
		t.Errorf("all green: %q", mark)
	}
	if mark := oracleMark(oracles); !strings.Contains(looks(mark), "fg=ansi3") {
		t.Errorf("some not green: %q", mark)
	}
	if !strings.Contains(orchestratorGuide, "neither `wasRed` nor `staysGreen`") || !strings.Contains(orchestratorGuide, "`--review KEY`") {
		t.Errorf("the orchestrator's guide doesn't say what to do with oracles, or how to have work reviewed:\n%s", orchestratorGuide)
	}
	// A task's and a ticket's agent are told to record them, without a permission prompt.
	task := taskPrompt("task:app-1", "t", "do it", agentClaude, "/w", "b", true, "")
	ticket := ticketPrompt(Dispatch{Issue: Issue{Key: "FLD-1"}, Agent: agentClaude}, "/w", "b", "the ticket", "")
	for _, p := range []string{task, ticket} {
		if !strings.Contains(p, "## Oracles") || !strings.Contains(p, "`foreman oracle \"<name>\" red") || !strings.Contains(p, "--stays-green") {
			t.Errorf("the prompt doesn't say how to record an oracle:\n%s", p)
		}
	}
}

const sampleDiff = `diff --git a/app.go b/app.go
index 1111111..2222222 100644
--- a/app.go
+++ b/app.go
@@ -10,3 +10,4 @@ func run() {
 	a := 1
-	b := 2
+	b := 3
+	c := 4
 	return a
@@ -40 +41 @@ func other() {
--- a dashed line that was removed
+++ a plus line that was added
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-first
-second
\ No newline at end of file
`

// A diff's lines know their file and their number, in the new file or for a removed line in the
// old, and a hunk's lines are counted off its header, whatever they look like.
func TestParseDiff(t *testing.T) {
	lines := parseDiff(sampleDiff)
	at := func(text string) diffLine {
		t.Helper()
		for _, l := range lines {
			if l.text == text {
				return l
			}
		}
		t.Fatalf("no line %q", text)
		return diffLine{}
	}
	for _, tc := range []struct {
		text  string
		hunk  bool
		where string
	}{
		{" 	a := 1", true, "app.go:10"},
		{"-	b := 2", true, "app.go, removed line 11"},
		{"+	b := 3", true, "app.go:11"},
		{"+	c := 4", true, "app.go:12"},
		{" 	return a", true, "app.go:13"},
		{"--- a dashed line that was removed", true, "app.go, removed line 40"},
		{"+++ a plus line that was added", true, "app.go:41"},
		{"-second", true, "gone.txt, removed line 2"},
	} {
		if l := at(tc.text); l.hunk != tc.hunk || l.where() != tc.where {
			t.Errorf("%q: hunk %t, %s; want %t, %s", tc.text, l.hunk, l.where(), tc.hunk, tc.where)
		}
	}
	for _, text := range []string{"diff --git a/app.go b/app.go", "+++ b/app.go", "@@ -10,3 +10,4 @@ func run() {", "\\ No newline at end of file", "+++ /dev/null"} {
		if at(text).hunk {
			t.Errorf("%q takes a comment; it is no line of the change", text)
		}
	}
}

// r on an agent shows its diff; comments go on lines of the change and reach the agent as one
// reply that names the file they are written to.
func TestReviewSendsCommentsAsOneReply(t *testing.T) {
	rex := useFakeRex(t)
	tempState(t)
	dir := t.TempDir()
	dispatched(t, "FLD-1", dir)
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session, d.ID = "s1", "a" }) })
	q := &queueModel{details: map[string][]string{}, width: 100, height: 30, state: loadState(),
		sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "idle", Block: "b1"}}}
	q.rebuild()
	press := func(key tea.KeyPressMsg) tea.Cmd {
		_, cmd := q.Update(key)
		return cmd
	}
	letter := func(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }
	typeText := func(text string) {
		for _, r := range text {
			press(letter(r))
		}
	}
	if cmd := press(letter('r')); cmd == nil || q.review == nil || !q.review.loading {
		t.Fatalf("r: no diff asked for")
	}
	q.Update(queueDiffMsg{key: "FLD-1", from: "abcdef123456", out: sampleDiff})
	r := q.review
	if r.loading || r.lines[r.cursor].text != " 	a := 1" {
		t.Fatalf("the cursor starts on %q; want the first line of the change", r.lines[r.cursor].text)
	}
	// A header takes no comment.
	press(tea.KeyPressMsg{Code: tea.KeyUp})
	if press(letter('c')); r.typing {
		t.Error("c on a hunk's header opened a comment")
	}
	press(tea.KeyPressMsg{Code: tea.KeyDown})
	press(tea.KeyPressMsg{Code: tea.KeyDown})
	press(tea.KeyPressMsg{Code: tea.KeyDown})
	press(letter('c'))
	typeText("why 3?")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	press(letter('n'))
	if r.lines[r.cursor].text != "--- a dashed line that was removed" {
		t.Fatalf("n went to %q; want the next hunk's first line", r.lines[r.cursor].text)
	}
	press(letter('c'))
	typeText("keep it")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	// One more, then dropped.
	press(tea.KeyPressMsg{Code: tea.KeyDown})
	press(letter('c'))
	typeText("x")
	press(tea.KeyPressMsg{Code: tea.KeyEnter})
	press(letter('x'))
	screen := ansi.Strip(q.content())
	for _, want := range []string{"Review · FLD-1 · since abcdef12 · 2 comments", "↳ keep it", "s send them to the agent"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the review lacks %q:\n%s", want, screen)
		}
	}
	if n := len(strings.Split(q.content(), "\n")); n != 30 {
		t.Errorf("the review is %d lines; want the pane's 30", n)
	}
	// esc with comments unsent asks first.
	if press(tea.KeyPressMsg{Code: tea.KeyEscape}); q.review == nil {
		t.Fatal("esc dropped the comments unasked")
	}
	send := press(letter('s'))
	if send == nil || q.review != nil {
		t.Fatalf("s: nothing sent, or the review still shows")
	}
	said := deliverNow(t, send)
	if said != "✓ Sent FLD-1 2 comments in its pane" {
		t.Errorf("said %q", said)
	}
	file := filepath.Join(evidenceDir("FLD-1"), "review.md")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Review of FLD-1's changes\n\n2 comments on the diff of your work since it started. Each names the file and line it is on, then shows the line.\n\n" +
		"## 1. app.go:11\n\n    +	b := 3\n\nwhy 3?\n\n## 2. app.go, removed line 40\n\n    --- a dashed line that was removed\n\nkeep it\n"
	if string(b) != want {
		t.Errorf("review.md:\n%s\nwant:\n%s", b, want)
	}
	typed := "send s1 b1 I reviewed your changes and left 2 comments on lines of the diff, in " + file + ". Read it and address each one."
	if len(rex.calls) != 2 || rex.calls[0] != typed || rex.calls[1] != "press s1 b1 enter" {
		t.Errorf("typed into the pane: %q", rex.calls)
	}
	if log := logLines(readLog("FLD-1")); len(log) != 3 || !strings.HasSuffix(log[2], "**me sends**: 2 comments on its diff") {
		t.Errorf("the log: %q", log)
	}

	// Nothing to review, and what r doesn't take.
	press(letter('r'))
	if q.Update(queueDiffMsg{key: "FLD-1"}); q.review != nil || !strings.Contains(q.status, "changed nothing") {
		t.Errorf("an empty diff: review %v, status %q", q.review, q.status)
	}
	if cmd := q.openReview(queueItem{Kind: agentItem, Key: "app#3", Dir: dir}); cmd != nil || q.review != nil {
		t.Error("r opened a review agent's diff")
	}
}

// deliverNow runs a sequence's first command, which sends, and returns the status it reports.
// bubbletea's sequence is a slice of commands of a type it keeps to itself.
func deliverNow(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	msg := cmd()
	for {
		if status, ok := msg.(queueStatusMsg); ok {
			return status.text
		}
		v := reflect.ValueOf(msg)
		if v.Kind() != reflect.Slice || v.Len() == 0 {
			t.Fatalf("no status from %T", msg)
		}
		first, ok := v.Index(0).Interface().(tea.Cmd)
		if !ok {
			t.Fatalf("no command in %T", msg)
		}
		msg = first()
	}
}

// foreman dispatch --review puts a reviewer on the commits an agent made since its dispatch,
// as a task in a worktree of its own at the head of that work.
func TestDispatchReviewsUnpushedWork(t *testing.T) {
	rex := useFakeRex(t)
	tempState(t)
	root := t.TempDir()
	oldRoots := repoRoots
	repoRoots = []string{root}
	t.Cleanup(func() { repoRoots = oldRoots })
	origin, repo := filepath.Join(root, "origin.git"), filepath.Join(root, "tool")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, repo, "remote", "add", "origin", origin)
	git(t, repo, "push", "-q", "origin", "main")
	if err := runDispatch([]string{"--repo", "tool", "--agent", "pi", "--", "Add a flag"}); err != nil {
		t.Fatal(err)
	}
	work := repo + "-task-1"
	if err := runDispatch([]string{"--review", "task:tool-1", "--agent", "pi"}); err == nil || !strings.Contains(err.Error(), "no commit since it was dispatched") {
		t.Errorf("a review of work with no commit: %v", err)
	}
	git(t, work, "commit", "-q", "--allow-empty", "-m", "add the flag")
	head := git(t, work, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(evidenceDir("task:tool-1"), "report.md"), []byte("# Added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The key in any case, as the other commands take it.
	if err := runDispatch([]string{"--review", "TASK:tool-1", "--agent", "pi", "look", "at", "the", "parsing"}); err != nil {
		t.Fatal(err)
	}
	d, ok := loadState().Dispatched["task:tool-2"]
	if !ok || d.Title != "Review task:tool-1: Add a flag" || d.Dir != repo+"-task-2" || d.StartedFrom != "" {
		t.Fatalf("the review's dispatch: %+v, %t", d, ok)
	}
	if got := git(t, d.Dir, "rev-parse", "HEAD"); got != head || git(t, d.Dir, "branch", "--show-current") != "" {
		t.Errorf("the review's worktree is on %s; want the work's head %s, detached", got, head)
	}
	if _, started := rex.cmds["task-tool-2-review-task-tool"]; !started {
		t.Errorf("sessions started: %v", rex.cmds)
	}
	b, err := os.ReadFile(promptFile("task:tool-2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`# task:tool-2: "Review task:tool-1: Add a flag"`,
		"reviewing what an agent did on task:tool-1, before any of it is pushed. Do not change it.",
		"..HEAD`, the 1 commit it made since it was dispatched.",
		"What it was asked: `" + promptFile("task:tool-1") + "`.",
		"What it says it did: `" + filepath.Join(evidenceDir("task:tool-1"), "report.md") + "`.",
		"## Extra context from me\n\nlook at the parsing",
		"Do not commit, push, or message the agent.",
		evidenceDir("task:tool-2") + "/report.md",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the review's prompt lacks %q:\n%s", want, b)
		}
	}
	// What --review doesn't take.
	_, _ = updateState(func(s *State) { s.Dispatched["tool#3"] = Dispatched{Dir: work} })
	for _, bad := range [][]string{
		{"--review", "task:tool-9"}, {"--review", "tool#3"}, {"--review", "task:tool-1", "--repo", "tool"},
		{"--review", "task:tool-1", "--ticket", "FLD-1"}, {"--review", "task:tool-1", "--title", "x"},
	} {
		if err := runDispatch(append(bad, "--agent", "pi")); err == nil {
			t.Errorf("foreman dispatch %v: want an error", bad)
		}
	}
}
