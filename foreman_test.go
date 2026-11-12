package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/zalando/go-keyring"
)

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		argv       []string
		jql, title string
	}{
		{nil, "project = FLD AND statusCategory != Done ORDER BY updated DESC", "FLD open tickets"},
		{[]string{"oo"}, "project = OO AND statusCategory != Done ORDER BY updated DESC", "OO open tickets"},
		{[]string{"assignee", "=", "currentUser()"}, "assignee = currentUser()", "custom JQL"},
	} {
		jql, title := parseArgs(tc.argv)
		if jql != tc.jql || title != tc.title {
			t.Errorf("parseArgs(%q) = %q, %q; want %q, %q", tc.argv, jql, title, tc.jql, tc.title)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"My van ALWAYS breaks!":                   "my-van-always-breaks",
		"Café — résumé: crème brûlée":             "cafe-resume-creme-brulee",
		"one two three four five six seven eight": "one-two-three-four-five-six",
		"  ---  ": "",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTicketBranch(t *testing.T) {
	remotes := []string{
		"origin/develop",
		"origin/li/FLD-1675-live", // newest first
		"origin/li/FLD-1675-old",
		"origin/rf/fld-16750-other",
		"upstream/FLD-1676-fork",
		"origin/rf/fld-1676-mine",
	}
	for key, want := range map[string]string{
		"FLD-1675": "origin/li/FLD-1675-live",
		"FLD-1676": "origin/rf/fld-1676-mine", // not the fork's
		"FLD-167":  "",                        // a prefix of another key
		"FLD-9999": "",
	} {
		if got := ticketBranch(remotes, key); got != want {
			t.Errorf("ticketBranch(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestBaseBranch(t *testing.T) {
	for _, tc := range []struct {
		remotes    []string
		originHead string
		want       string
	}{
		{[]string{"origin/main", "origin/develop"}, "origin/main", "origin/develop"},
		{[]string{"origin/main", "origin/trunk"}, "origin/trunk", "origin/trunk"},
		{[]string{"origin/master"}, "", "origin/master"},
		{[]string{"origin/feature"}, "", ""},
	} {
		if got := baseBranch(tc.remotes, tc.originHead); got != tc.want {
			t.Errorf("baseBranch(%v, %q) = %q, want %q", tc.remotes, tc.originHead, got, tc.want)
		}
	}
}

func rollup(state string) prNode {
	var p prNode
	if state != "" {
		p.Commits.Nodes = make([]struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		}, 1)
		p.Commits.Nodes[0].Commit.StatusCheckRollup = &struct {
			State string `json:"state"`
		}{state}
	}
	return p
}

func TestMyStatus(t *testing.T) {
	approved := rollup("SUCCESS")
	approved.ReviewDecision = "APPROVED"
	approvedRunning := rollup("PENDING")
	approvedRunning.ReviewDecision = "APPROVED"
	conflict := rollup("SUCCESS")
	conflict.Mergeable = "CONFLICTING"
	changes := rollup("")
	changes.ReviewDecision = "CHANGES_REQUESTED"
	for _, tc := range []struct {
		name          string
		draft         bool
		node          prNode
		status, categ string
	}{
		{"approved, CI passed", false, approved, "approved ✓", "Done"},
		{"approved, CI running", false, approvedRunning, "approved ●", "In Progress"},
		{"CI failed", false, rollup("FAILURE"), "in review ✗", "Blocked"},
		{"conflict", false, conflict, "conflict ✓", "Blocked"},
		{"changes, no CI", false, changes, "changes", "Blocked"},
		{"draft", true, rollup(""), "draft", "To Do"},
	} {
		st, cat := myStatus(tc.draft, tc.node)
		if st != tc.status || cat != tc.categ {
			t.Errorf("%s: myStatus = %q, %q; want %q, %q", tc.name, st, cat, tc.status, tc.categ)
		}
	}
}

func TestJiraKey(t *testing.T) {
	v := &PrView{HeadRefName: "li/fld-1675-feedback"}
	if got := jiraKey(Pr{Summary: "Fix reports"}, v); got != "FLD-1675" {
		t.Errorf("key from branch = %q", got)
	}
	if got := jiraKey(Pr{Summary: "[OO-12] Fix"}, v); got != "OO-12" {
		t.Errorf("title wins over branch: %q", got)
	}
	if got := jiraKey(Pr{Summary: "Bump deps"}, nil); got != "" {
		t.Errorf("no key = %q", got)
	}
}

func TestChecks(t *testing.T) {
	var v PrView
	if checks(v) != nil {
		t.Fatal("no checks should give no section")
	}
	v.StatusCheckRollup = []struct {
		Name       string `json:"name"`
		Context    string `json:"context"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	}{
		{Name: "lint", Conclusion: "SUCCESS"},
		{Name: "test", Conclusion: "FAILURE"},
		{Context: "deploy", State: "PENDING"},
	}
	got := strings.Join(checks(v), "\n")
	for _, want := range []string{"✗ **test** failure", "● deploy running", "1/3 passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("checks missing %q:\n%s", want, got)
		}
	}
}

func TestFilterRows(t *testing.T) {
	rows := []Row{
		issueRow(Issue{Key: "FLD-1", Summary: "Fix the van", Status: "To Do", Assignee: "Ricardo", Mine: true}),
		issueRow(Issue{Key: "FLD-2", Summary: "Van report", Status: "In Progress", Assignee: "Ana"}),
		issueRow(Issue{Key: "FLD-3", Summary: "Other", Status: "To Do"}),
		prRow(Pr{Key: "app#1", Summary: "Review me", Requested: true}),
		prRow(Pr{Key: "app#2", Summary: "Mine", Mine: true}),
	}
	keys := func(rs []Row) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Key)
		}
		return out
	}
	unassigned := ""
	for _, tc := range []struct {
		name     string
		query    string
		prFilter string
		mine     bool
		assignee *string
		want     []string
	}{
		{"every word must match", "van fix", "all", false, nil, []string{"FLD-1"}},
		{"matches status and assignee", "progress ana", "all", false, nil, []string{"FLD-2"}},
		{"mine as Jira says", "", "all", true, nil, []string{"FLD-1", "app#1", "app#2"}},
		{"unassigned", "", "all", false, &unassigned, []string{"FLD-3", "app#1", "app#2"}},
		{"requested PRs", "", "requested", false, nil, []string{"FLD-1", "FLD-2", "FLD-3", "app#1"}},
	} {
		got := keys(filterRows(rows, tc.query, tc.prFilter, tc.mine, tc.assignee))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The five-minute refresh reorders the rows under the cursor.
func TestReloadKeepsTheCursorOnItsRow(t *testing.T) {
	rows := func(keys ...string) []Row {
		var out []Row
		for _, k := range keys {
			out = append(out, issueRow(Issue{Key: k}))
		}
		return out
	}
	m := &model{
		prFilter: "all",
		inFlight: map[viewKind]bool{},
		lastLoad: map[viewKind]time.Time{},
		details:  map[string][]string{},
		detailOK: map[string]bool{},
		issues:   rows("FLD-1", "FLD-2", "FLD-3"),
		cursor:   1,
	}
	m.Update(loadedMsg{which: tickets, rows: rows("FLD-4", "FLD-3", "FLD-1", "FLD-2")})
	if row, _ := m.current(); row.Key != "FLD-2" {
		t.Errorf("cursor on %s after the reload, want FLD-2", row.Key)
	}
	// The other view's reload leaves it alone.
	m.Update(loadedMsg{which: prs, rows: []Row{prRow(Pr{Key: "app#1"})}})
	if row, _ := m.current(); row.Key != "FLD-2" {
		t.Errorf("cursor on %s after the PRs loaded, want FLD-2", row.Key)
	}
}

func TestMostUrgent(t *testing.T) {
	for want, in := range map[string][]string{
		"blocked": {"working", "blocked", "idle"},
		"working": {"idle", "working"},
		"open":    nil,
	} {
		if got := mostUrgent(in); got != want {
			t.Errorf("mostUrgent(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestUpdateState(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "sub", "state.json")
	t.Cleanup(func() { stateFile = old })

	if _, err := updateState(func(s *State) { s.Dispatched["FLD-1"] = "/tmp/x" }); err != nil {
		t.Fatal(err)
	}
	if _, err := updateState(func(s *State) { s.Sessions["FLD-1"] = "fld-1-x" }); err != nil {
		t.Fatal(err)
	}
	s := loadState()
	if s.Dispatched["FLD-1"] != "/tmp/x" || s.Sessions["FLD-1"] != "fld-1-x" {
		t.Errorf("updates lost: %+v", s)
	}

	if err := os.WriteFile(stateFile, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := loadState(); len(s.Dispatched) != 0 {
		t.Errorf("corrupt file should load empty, got %+v", s)
	}
	if !exists(stateFile + ".corrupt") {
		t.Error("corrupt file should be kept aside")
	}
}

// The board and the queue are separate processes that both rewrite the state file: an update one
// makes between another's read and write must survive.
func TestUpdateStateAcrossProcesses(t *testing.T) {
	const writers, each = 4, 50
	if name := os.Getenv("FOREMAN_TEST_STATE_WRITER"); name != "" {
		stateFile = os.Getenv("FOREMAN_TEST_STATE_FILE")
		for n := range each {
			if _, err := updateState(func(s *State) { s.Dispatched[fmt.Sprintf("%s-%d", name, n)] = "/w" }); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })

	var cmds []*exec.Cmd
	for n := range writers {
		cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateStateAcrossProcesses$")
		cmd.Env = append(os.Environ(), fmt.Sprintf("FOREMAN_TEST_STATE_WRITER=W%d", n), "FOREMAN_TEST_STATE_FILE="+stateFile)
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
	if got := len(loadState().Dispatched); got != writers*each {
		t.Errorf("%d of %d updates survived", got, writers*each)
	}
}

// Quitting during the browser sign-in mustn't wait for it: the connect holds the lock throughout.
func TestJiraCloseDoesNotWaitForConnect(t *testing.T) {
	j := &Jira{}
	j.mu.Lock() // a connect in flight
	defer j.mu.Unlock()
	done := make(chan struct{})
	go func() {
		j.close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close waited for the connect")
	}
}

// git runs git in dir for test setup.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPrepareWorktreeKeepsLocalCommits: an existing local ticket branch with unpushed commits is
// checked out as is, not reset to origin's.
func TestPrepareWorktreeKeepsLocalCommits(t *testing.T) {
	root := t.TempDir()
	origin, repo := filepath.Join(root, "origin"), filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "develop", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, origin, "branch", "li/FLD-7-thing")
	git(t, root, "clone", "-q", origin, repo)
	git(t, repo, "branch", "li/FLD-7-thing", "origin/li/FLD-7-thing")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "unpushed")
	git(t, repo, "branch", "-f", "li/FLD-7-thing", "HEAD")
	git(t, repo, "reset", "-q", "--hard", "origin/develop")
	unpushed := git(t, repo, "rev-parse", "li/FLD-7-thing")

	cwd := repo + "-fld-7"
	_, branch, err := prepareWorktree(repo, cwd, Issue{Key: "FLD-7", Summary: "Thing"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if branch != "li/FLD-7-thing" {
		t.Errorf("branch = %q", branch)
	}
	if got := git(t, cwd, "rev-parse", "HEAD"); got != unpushed {
		t.Errorf("worktree HEAD = %s, want the unpushed %s", got, unpushed)
	}
}

func TestPrepareWorktreeNewBranch(t *testing.T) {
	root := t.TempDir()
	origin, repo := filepath.Join(root, "origin"), filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "main", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, root, "clone", "-q", origin, repo)

	cwd := repo + "-fld-8"
	_, branch, err := prepareWorktree(repo, cwd, Issue{Key: "FLD-8", Summary: "New thing"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if branch != "rf/fld-8-new-thing" {
		t.Errorf("branch = %q", branch)
	}
	if got, want := git(t, cwd, "rev-parse", "HEAD"), git(t, repo, "rev-parse", "origin/main"); got != want {
		t.Errorf("new branch starts at %s, want origin/main %s", got, want)
	}
}

// A ticket branch checked out in a worktree made by hand is worked on there; git won't check it out twice.
func TestPrepareWorktreeReusesTheBranchsWorktree(t *testing.T) {
	root := t.TempDir()
	origin, repo := filepath.Join(root, "origin"), filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "develop", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, origin, "branch", "rf/fld-9-accents")
	git(t, root, "clone", "-q", origin, repo)
	byHand := filepath.Join(root, "repo-accents")
	git(t, repo, "worktree", "add", "-q", "--track", "-b", "rf/fld-9-accents", byHand, "origin/rf/fld-9-accents")

	dir, branch, err := prepareWorktree(repo, repo+"-fld-9", Issue{Key: "FLD-9", Summary: "Accents"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	// macOS temp dirs sit behind a symlink; git reports the resolved path.
	want, _ := filepath.EvalSymlinks(byHand)
	if got, _ := filepath.EvalSymlinks(dir); got != want || branch != "rf/fld-9-accents" {
		t.Errorf("got %s on %s, want %s on rf/fld-9-accents", dir, branch, byHand)
	}
	if exists(repo + "-fld-9") {
		t.Error("created a second worktree")
	}
}

func TestErrorLinesPutsTheFailureFirst(t *testing.T) {
	got := errorLines("Preparing worktree (checking out 'b')\nfatal: 'b' is already used by worktree at '/w'")
	if firstLine(got) != "fatal: 'b' is already used by worktree at '/w'" {
		t.Errorf("first line = %q", firstLine(got))
	}
}

// TestCallbackServer: requests without the sign-in's state are refused and don't end it; an error
// from the authorization server is escaped in the page and ends it.
func TestCallbackServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	codes, err := waitForCode(ctx, "s3cret")
	if err != nil {
		t.Skipf("callback port busy: %v", err)
	}
	get := func(query string) (int, string) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?%s", callbackPort, query))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if status, _ := get("error=" + url.QueryEscape("<script>x</script>")); status != http.StatusBadRequest {
		t.Errorf("no state: status %d, want 400", status)
	}
	select {
	case r := <-codes:
		t.Fatalf("a request without the state ended the sign-in: %+v", r)
	default:
	}

	_, body := get("state=s3cret&error=" + url.QueryEscape("<script>x</script>"))
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("error not escaped: %s", body)
	}
	if r := <-codes; r.err == nil || !strings.Contains(r.err.Error(), "<script>x</script>") {
		t.Errorf("result = %+v, want the error", r)
	}
}

func TestQueueItems(t *testing.T) {
	old := evidenceRoot
	evidenceRoot = t.TempDir()
	t.Cleanup(func() { evidenceRoot = old })
	report := func(key string, files ...string) {
		dir := evidenceDir(key)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range append(files, "report.md") {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("# done\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	report("FLD-1", "01-list.png", "demo.webm", "tui.txt")
	report("repo#7")
	report("FLD-4")
	s := State{
		Dispatched: map[string]string{"FLD-1": "/w/1", "FLD-2": "/w/2", "FLD-3": "/w/3", "FLD-4": "/w/4", "repo#7": "/w/7", "FLD-5": "/w/5"},
		Sessions:   map[string]string{"FLD-1": "s1", "FLD-2": "s2", "FLD-3": "s3", "FLD-4": "s4", "repo#7": "s7", "FLD-5": "s5"},
		Titles:     map[string]string{},
		Dismissed:  map[string]string{},
		Hidden:     map[string]bool{},
	}
	// FLD-2 asks, FLD-3 works, FLD-5 finished without a report, FLD-4's session is closed.
	sessions := []rexSession{{"s1", "fld-1-a", "done", nil}, {"s2", "fld-2-b", "blocked", nil}, {"s3", "fld-3-c", "working", nil}, {"s7", "review-repo-7", "idle", nil}, {"s5", "fld-5-e", "done", nil}}
	prs := []Pr{
		{Key: "repo#9", Mine: true, Category: "Blocked", Status: "in review ✗"},
		{Key: "repo#10", Mine: true, Category: "Done", Status: "approved ✓"},
		{Key: "repo#11", Category: "Blocked"},
	}
	groups := func(items []queueItem) map[string]group {
		out := map[string]group{}
		for _, it := range items {
			out[it.Key] = it.Group
		}
		return out
	}
	items := queueItems(s, sessions, prs)
	want := map[string]group{"FLD-2": needsYou, "repo#9": needsYou, "FLD-1": toReview, "FLD-4": toReview, "repo#7": toReview, "repo#10": toReview, "FLD-3": working, "FLD-5": idle}
	if got := groups(items); !maps.Equal(got, want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	// Agents that ask come before my PRs.
	if items[0].Key != "FLD-2" || items[1].Key != "repo#9" || items[len(items)-1].Key != "FLD-5" {
		t.Errorf("order = %v", items)
	}
	first := items[slices.IndexFunc(items, func(it queueItem) bool { return it.Key == "FLD-1" })]
	if first.Shots != 2 || first.Videos != 1 {
		t.Errorf("FLD-1 evidence = %d shots, %d videos", first.Shots, first.Videos)
	}

	// Marking reviewed moves a report down to idle, or closed once its session goes; it stays listed.
	s.Dismissed["FLD-1"] = first.Sig
	if got := groups(queueItems(s, sessions, prs))["FLD-1"]; got != idle {
		t.Errorf("reviewed FLD-1 = %v, want idle", got)
	}
	sessions = slices.DeleteFunc(sessions, func(r rexSession) bool { return r.ID == "s1" })
	if got, ok := groups(queueItems(s, sessions, prs))["FLD-1"]; !ok || got != closed {
		t.Errorf("FLD-1 with its session closed = %v (listed %t), want closed", got, ok)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(evidenceDir("FLD-1"), "report.md"), later, later); err != nil {
		t.Fatal(err)
	}
	if got := groups(queueItems(s, sessions, prs))["FLD-1"]; got != toReview {
		t.Errorf("FLD-1 with a new report = %v, want review", got)
	}
	// A reviewed PR goes until its status changes.
	s.Dismissed["pr:repo#9"] = "in review ✗"
	if _, ok := groups(queueItems(s, sessions, prs))["repo#9"]; ok {
		t.Error("reviewed repo#9 still listed")
	}

	// A review agent and its PR share a key; marking the PR reviewed leaves the agent's report.
	s.Dispatched["repo#9"] = "/w/9"
	report("repo#9")
	pr := queueItem{Kind: prItem, Key: "repo#9"}
	s.Dismissed[pr.dismissKey()] = "in review ✗"
	if got := groups(queueItems(s, sessions, prs))["repo#9"]; got != toReview {
		t.Errorf("repo#9's agent = %v after its PR was marked reviewed, want review", got)
	}

	// A closed agent can be dropped; it shows again when its session reopens.
	s.Hidden["FLD-4"] = true
	if _, ok := groups(queueItems(s, sessions, prs))["FLD-4"]; ok {
		t.Error("hidden FLD-4 still listed")
	}
	sessions = append(sessions, rexSession{"s4", "fld-4-d", "idle", nil})
	if _, ok := groups(queueItems(s, sessions, prs))["FLD-4"]; !ok {
		t.Error("FLD-4 not listed with its session open again")
	}
}

// The list is rebuilt on every poll while I pick a row or type a reply.
func TestQueueKeepsItsPlaceAcrossRebuilds(t *testing.T) {
	old := evidenceRoot
	evidenceRoot = t.TempDir()
	t.Cleanup(func() { evidenceRoot = old })
	q := &queueModel{
		details: map[string][]string{},
		state: State{
			Dispatched: map[string]string{"FLD-1": "/w/1", "FLD-2": "/w/2", "repo#9": "/w/9"},
			Sessions:   map[string]string{"FLD-1": "s1", "FLD-2": "s2", "repo#9": "s9"},
		},
		sessions: []rexSession{{"s1", "fld-1-a", "idle", nil}, {"s2", "fld-2-b", "idle", nil}, {"s9", "review-repo-9", "working", nil}},
		prs:      []Pr{{Key: "repo#9", Mine: true, Category: "Blocked", Status: "changes"}},
	}
	q.rebuild()
	at := func(key string, kind itemKind) int {
		return slices.IndexFunc(q.items, func(it queueItem) bool { return it.Key == key && it.Kind == kind })
	}

	// A review agent shares its key with the PR it reviews, which sorts above it.
	q.cursor = at("repo#9", agentItem)
	q.rebuild()
	if it, _ := q.current(); it.Kind != agentItem || it.Key != "repo#9" {
		t.Errorf("the cursor moved from repo#9's agent to %+v", it)
	}

	// A reply started on FLD-1 goes to FLD-1, even if its row is gone by the time it is sent.
	q.cursor = at("FLD-1", agentItem)
	q.input(tea.KeyPressMsg{Code: 'f', Text: "f"})
	q.feedbackInput(tea.KeyPressMsg{Code: 'o', Text: "o"})
	delete(q.state.Dispatched, "FLD-1")
	q.rebuild()
	if it, _ := q.current(); it.Key == "FLD-1" {
		t.Fatal("FLD-1 is still listed")
	}
	if r := q.feedback; r == nil || r.key != "FLD-1" || r.session != "s1" || r.text != "o" {
		t.Fatalf("reply = %+v, want FLD-1's in s1", r)
	}
	q.feedbackInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	if q.feedback != nil || !strings.Contains(q.status, "FLD-1") {
		t.Errorf("after enter: reply %+v, status %q", q.feedback, q.status)
	}
}

func TestFindSession(t *testing.T) {
	sessions := []rexSession{
		{"session:a", "fld-1704", "done", nil},
		{"session:b", "review-reports-app-1185-x", "idle", nil},
		{"session:d", "fix-reports-app-1185-address-review", "idle", nil},
		{"session:c", "fld-17040-other", "working", nil},
	}
	for _, tc := range []struct{ key, recorded, want string }{
		{"FLD-1704", "session:c", "session:c"},                 // the recorded ID wins
		{"FLD-1704", "fld-1704-test-work-orders", "session:a"}, // renamed: matched by the key
		{"reports-app#1185", "gone", "session:b"},
		{"fix:reports-app#1185", "gone", "session:d"}, // a fix's session isn't its review's
		{"FLD-170", "", ""},                           // a prefix of another key doesn't match
		{"FLD-1679", "", ""},
	} {
		got, _ := findSession(sessions, tc.key, tc.recorded)
		if got.ID != tc.want {
			t.Errorf("findSession(%s, %s) = %q, want %q", tc.key, tc.recorded, got.ID, tc.want)
		}
	}
	s := State{Dispatched: map[string]string{"FLD-1704": "/w", "FLD-1679": "/w"}, Sessions: map[string]string{"FLD-1704": "fld-1704-test-work-orders"}}
	if got := sessionFixes(s, sessions); !maps.Equal(got, map[string]string{"FLD-1704": "session:a"}) {
		t.Errorf("sessionFixes = %v", got)
	}
}

func TestEvidenceDir(t *testing.T) {
	if got := filepath.Base(evidenceDir("Fleet-API#412")); got != "fleet-api-412" {
		t.Errorf("evidenceDir = %q", got)
	}
}

// An agents result must not schedule another poll: focus and dispatch poll too, and each extra
// reschedule would start another loop of `rex do` calls.
func TestAgentsResultDoesNotReschedule(t *testing.T) {
	m := &model{agents: map[string]string{}}
	if _, cmd := m.Update(agentsMsg{states: map[string]string{"fld-1": "working"}}); cmd != nil {
		t.Error("agentsMsg returned a command")
	}
	if m.agents["fld-1"] != "working" {
		t.Errorf("agents = %v", m.agents)
	}
	// It carries the state file too, which the queue and other boards write; a failed listing of
	// the sessions keeps the last one.
	m.Update(agentsMsg{state: State{Dispatched: map[string]string{"FLD-9": "/w"}}})
	if m.state.Dispatched["FLD-9"] != "/w" || m.agents["fld-1"] != "working" {
		t.Errorf("state = %+v, agents = %v", m.state, m.agents)
	}
	q := &queueModel{details: map[string][]string{}}
	if _, cmd := q.Update(queueStateMsg{State{}, nil, true}); cmd != nil {
		t.Error("queueStateMsg with no items returned a command")
	}
}

// A Jira or GitHub call that failed isn't a ticket or PR that is still open.
func TestCleanupReportsWhatItCouldNotCheck(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })
	failed := []string{"FLD-1: not signed in", "FLD-2: not signed in"}

	m := &model{}
	m.openCleanup(cleanupMsg{failed: failed})
	if m.cleanup != nil || m.statusColor != cRed || !strings.Contains(m.status, "2 of the dispatched worktrees: FLD-1: not signed in") {
		t.Errorf("nothing found, two unchecked: status %q, view %v", m.status, m.cleanup)
	}
	m.openCleanup(cleanupMsg{candidates: []cleanupCandidate{{Key: "FLD-3", Dir: "/w/3"}}, failed: failed})
	if m.cleanup == nil || !strings.Contains(m.status, "FLD-1: not signed in") {
		t.Errorf("one found, two unchecked: status %q, view %v", m.status, m.cleanup)
	}
	m.openCleanup(cleanupMsg{})
	if m.statusColor != cGreen {
		t.Errorf("all checked, nothing found: status %q", m.status)
	}
}

// tempState points the state file and the evidence at a temporary directory.
func tempState(t *testing.T) {
	t.Helper()
	oldState, oldEvidence := stateFile, evidenceRoot
	dir := t.TempDir()
	stateFile, evidenceRoot = filepath.Join(dir, "state.json"), filepath.Join(dir, "evidence")
	t.Cleanup(func() { stateFile, evidenceRoot = oldState, oldEvidence })
}

// dispatched records a dispatch with a report, as one that ran would leave it.
func dispatched(t *testing.T, key, dir string) {
	t.Helper()
	if _, err := updateState(func(s *State) { s.Dispatched[key], s.Titles[key] = dir, key }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evidenceDir(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir(key), "report.md"), []byte("# done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Cleaning up removes a worktree, but of a dispatch that ran in the main checkout only the record.
func TestCleanupKeepsTheMainCheckout(t *testing.T) {
	tempState(t)
	root := t.TempDir()
	repo, worktree := filepath.Join(root, "repo"), filepath.Join(root, "repo-fld-1")
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, repo, "worktree", "add", "-q", worktree)

	for dir, wantMain := range map[string]bool{repo: true, worktree: false} {
		owns, main, err := owner(dir)
		if err != nil {
			t.Fatal(err)
		}
		// The temporary directory is behind a symlink on macOS.
		want, _ := filepath.EvalSymlinks(repo)
		if got, _ := filepath.EvalSymlinks(owns); got != want || main != wantMain {
			t.Errorf("owner(%s) = %s, main %t; want %s, main %t", dir, got, main, want, wantMain)
		}
	}

	dispatched(t, "FLD-1", worktree)
	dispatched(t, "FLD-2", repo)
	dispatched(t, "FLD-3", repo)
	m := &model{cleanup: &cleanupView{
		items: []cleanupCandidate{
			{Key: "FLD-1", Dir: worktree, Repo: repo},
			{Key: "FLD-2", Dir: repo, Repo: repo, Main: true},
			{Key: "FLD-3", Dir: repo, Repo: repo, Main: true},
		},
		picked: map[int]bool{0: true, 1: true},
	}}
	msg := m.removeWorktrees()().(cleanedMsg)
	if msg.removed != 1 || msg.forgotten != 1 || len(msg.errs) != 0 {
		t.Fatalf("cleaned = %+v", msg)
	}
	if exists(worktree) {
		t.Error("the worktree is still there")
	}
	if !exists(filepath.Join(repo, ".git")) {
		t.Error("the main checkout is gone")
	}
	if s := loadState(); !maps.Equal(s.Dispatched, map[string]string{"FLD-3": repo}) {
		t.Errorf("dispatched = %v, want only the unticked FLD-3", s.Dispatched)
	}
	if exists(evidenceDir("FLD-1")) || exists(evidenceDir("FLD-2")) || !exists(evidenceDir("FLD-3")) {
		t.Error("the reports of the cleaned dispatches should go, FLD-3's stay")
	}
}

// A worktree removed by hand has nothing left to list its report.
func TestCleanupDiscardsGoneWorktrees(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", filepath.Join(t.TempDir(), "gone"))
	m := &model{state: loadState()}
	if msg := m.findCleanup()().(cleanupMsg); len(msg.candidates) != 0 || len(msg.failed) != 0 || msg.err != nil {
		t.Fatalf("cleanup = %+v", msg)
	}
	if s := loadState(); len(s.Dispatched) != 0 || len(s.Titles) != 0 {
		t.Errorf("state = %+v", s)
	}
	if exists(evidenceDir("FLD-1")) {
		t.Error("its report is still there")
	}
}

// A second agent on a ticket whose first is still open takes a second enter.
func TestDispatchAsksBeforeASecondAgent(t *testing.T) {
	tempState(t)
	m := &model{
		state:    State{Sessions: map[string]string{"FLD-1": "s1"}, RepoByProject: map[string]string{}},
		agents:   map[string]string{"s1": "working"},
		repos:    []string{"/r/one"},
		details:  map[string][]string{},
		detailOK: map[string]bool{},
	}
	enter, down := tea.KeyPressMsg{Code: tea.KeyEnter}, tea.KeyPressMsg{Code: tea.KeyDown}
	m.openForm(issueRow(Issue{Key: "FLD-1"}))
	if cmd := m.formInput(enter); cmd != nil || m.form.busy || !strings.Contains(m.status, "still open (working)") {
		t.Fatalf("first enter: cmd %v, busy %t, status %q", cmd != nil, m.form.busy, m.status)
	}
	// Any other key takes the confirmation back.
	m.formInput(down)
	if cmd := m.formInput(enter); cmd != nil || m.form.busy {
		t.Fatal("enter after another key dispatched")
	}
	if cmd := m.formInput(enter); cmd == nil || !m.form.busy {
		t.Fatal("the second enter didn't dispatch")
	}

	// A ticket without an open session goes at once.
	m.openForm(issueRow(Issue{Key: "FLD-2"}))
	if cmd := m.formInput(enter); cmd == nil || !m.form.busy {
		t.Fatal("FLD-2 didn't dispatch on the first enter")
	}
}

func TestTruncatedPrList(t *testing.T) {
	m := &model{
		view: prs, prFilter: "all", width: 80, height: 20,
		inFlight: map[viewKind]bool{}, lastLoad: map[viewKind]time.Time{},
		details: map[string][]string{}, detailOK: map[string]bool{},
	}
	m.Update(loadedMsg{which: prs, rows: []Row{prRow(Pr{Key: "app#1"})}, truncated: true})
	if !strings.Contains(m.status, "first 500 open PRs") {
		t.Errorf("status = %q", m.status)
	}
	if header := firstLine(m.View().Content); !strings.Contains(header, "1/1+") {
		t.Errorf("header = %q", header)
	}
	m.Update(loadedMsg{which: prs, rows: []Row{prRow(Pr{Key: "app#1"})}})
	if header := firstLine(m.View().Content); strings.Contains(header, "+") || m.status != "" {
		t.Errorf("after a full load: header %q, status %q", header, m.status)
	}
}

func TestStaleDismissals(t *testing.T) {
	s := State{
		Dispatched: map[string]string{"FLD-1": "/w/1"},
		Dismissed:  map[string]string{"FLD-1": "a", "FLD-9": "b", "pr:repo#2": "c", "pr:repo#3": "d", "repo#2": "e"},
	}
	prs := []Pr{{Key: "repo#2", Mine: true, Category: "Blocked"}, {Key: "repo#3", Mine: true, Category: "In Progress"}}
	got := staleDismissals(s, prs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"FLD-9", "pr:repo#3", "repo#2"}) {
		t.Errorf("staleDismissals = %v", got)
	}
}

func TestLoadConfig(t *testing.T) {
	oldFile, oldSite, oldPrefix, oldProject := configFile, site, branchPrefix, defaultProject
	t.Cleanup(func() { configFile, site, branchPrefix, defaultProject = oldFile, oldSite, oldPrefix, oldProject })
	configFile = filepath.Join(t.TempDir(), "config.json")
	if err := loadConfig(); err != nil || site != oldSite {
		t.Fatalf("no file: err %v, site %q", err, site)
	}
	if err := os.WriteFile(configFile, []byte(`{"site": "https://x.atlassian.net/", "branchPrefix": "me/"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if site != "https://x.atlassian.net" || branchPrefix != "me/" || defaultProject != oldProject {
		t.Errorf("site %q, prefix %q, project %q", site, branchPrefix, defaultProject)
	}
	if err := os.WriteFile(configFile, []byte(`{"sit": "typo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(); err == nil {
		t.Error("an unknown key should be an error")
	}
}

// The prompt reaches the agent as one argument, whatever it holds, from a file larger than Rex's
// 64 KB request limit.
func TestAgentCommandPassesThePrompt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "prompt.md")
	prompt := "# FLD-1: \"x\" $HOME `y`\n\n" + strings.Repeat("a", 200_000)
	if err := os.WriteFile(file, []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := agentCommand("pi", file)
	// Stand in for pi: print the last argument.
	cmd[len(cmd)-1] = "/usr/bin/printf"
	cmd = append(cmd, "%s")
	out, err := exec.Command(cmd[0], cmd[1:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != prompt {
		t.Errorf("got %d bytes, want the %d-byte prompt", len(out), len(prompt))
	}
}

// Claude's --add-dir and --mcp-config take lists: the prompt after them would be read as one more.
func TestAgentCommandEndsClaudeOptions(t *testing.T) {
	for _, agent := range []string{agentClaude, "Claude (yolo)"} {
		cmd := agentCommand(agent, "prompt.md")
		if cmd[len(cmd)-1] != "--" {
			t.Errorf("%s: %q should end with --", agent, cmd)
		}
	}
}

// Atlassian's tokens are about 9 KB, over what one Keychain write takes, so the sign-in is split.
func TestStoredSplitsAcrossKeychainItems(t *testing.T) {
	keyring.MockInit()
	big := stored{Tokens: &storedTokens{AccessToken: strings.Repeat("a", 4300), RefreshToken: strings.Repeat("r", 3600)}}
	if err := saveStored(big); err != nil {
		t.Fatal(err)
	}
	if n := storedParts(); n < 4 {
		t.Fatalf("parts = %d", n)
	}
	for n := 1; n <= storedParts(); n++ {
		if part, _ := keyring.Get(keychainService, partAccount(n)); len(part) > keychainPart {
			t.Errorf("part %d is %d bytes", n, len(part))
		}
	}
	if got := loadStored(); got.Tokens == nil || got.Tokens.AccessToken != big.Tokens.AccessToken || got.Tokens.RefreshToken != big.Tokens.RefreshToken {
		t.Fatal("the split sign-in didn't load back")
	}
	// A smaller sign-in drops the parts it no longer needs.
	if err := saveStored(stored{Tokens: &storedTokens{AccessToken: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keychainService, partAccount(2)); err == nil {
		t.Error("part 2 left behind")
	}
	if got := loadStored(); got.Tokens == nil || got.Tokens.AccessToken != "x" {
		t.Errorf("loaded %+v", got.Tokens)
	}
	// An entry from before the split, the JSON itself, still loads.
	signOut()
	_ = keyring.Set(keychainService, keychainAccount, `{"tokens":{"access_token":"old"}}`)
	if got := loadStored(); got.Tokens == nil || got.Tokens.AccessToken != "old" {
		t.Errorf("legacy entry loaded %+v", got.Tokens)
	}
}

// poteto-mode is the second option: off when the form opens, and space on its row turns it on
// without touching the worktree option.
func TestFormTogglesPotetoMode(t *testing.T) {
	m := &model{state: State{RepoByProject: map[string]string{}}, repos: []string{"/r/one"}}
	tab, space := tea.KeyPressMsg{Code: tea.KeyTab}, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	m.openForm(issueRow(Issue{Key: "FLD-1"}))
	f := m.form
	if f.poteto {
		t.Fatal("poteto-mode starts on")
	}
	m.formInput(tab)
	m.formInput(tab)
	m.formInput(space)
	if !f.poteto || !f.worktree {
		t.Fatalf("after tab, tab, space: poteto %t, worktree %t", f.poteto, f.worktree)
	}
	if form := strings.Join(m.renderForm(100, 40), "\n"); !strings.Contains(form, fg(cGreen, "[x]")+" poteto-mode") {
		t.Errorf("the form doesn't show poteto-mode ticked:\n%s", form)
	}
	// Two more tabs reach the context, where a space is text.
	m.formInput(tab)
	m.formInput(tab)
	m.formInput(space)
	if f.context != " " || !f.poteto {
		t.Errorf("context %q, poteto %t", f.context, f.poteto)
	}

	// A PR's one option is fix: tab goes from the agent to it, to the context and back.
	m.openForm(prRow(Pr{Key: "o/r#1"}))
	m.formInput(tab)
	m.formInput(tab)
	m.formInput(space)
	m.formInput(tab)
	if f := m.form; f.context != " " || f.poteto || f.fix || f.field != fieldAgent {
		t.Errorf("review form: context %q, poteto %t, fix %t, field %d", f.context, f.poteto, f.fix, f.field)
	}
}

// fix it on a PR's form turns the review into a fix, on the PR's branch.
func TestFormTogglesFix(t *testing.T) {
	m := &model{state: State{RepoByProject: map[string]string{}}, repos: []string{"/r/one"}}
	tab, space := tea.KeyPressMsg{Code: tea.KeyTab}, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	m.openForm(prRow(Pr{Key: "app#1", Checkout: "/w/app", Number: 1, Branch: "li/fld-1-x"}))
	if form := strings.Join(m.renderForm(140, 40), "\n"); !strings.Contains(form, "/w/app-pr-1") || !strings.Contains(form, "enter review") {
		t.Errorf("a review form:\n%s", form)
	}
	m.formInput(tab)
	m.formInput(space)
	if !m.form.fix || m.form.worktree != true {
		t.Fatalf("after tab, space: fix %t", m.form.fix)
	}
	form := strings.Join(m.renderForm(140, 40), "\n")
	for _, want := range []string{fg(cGreen, "[x]") + " fix it", "li/fld-1-x", "/w/app-fix-1", "enter fix"} {
		if !strings.Contains(form, want) {
			t.Errorf("the fix form lacks %q:\n%s", want, form)
		}
	}
}

// The prompt names the skill only when the dispatch asks for it, ahead of the workflow that overrides it.
func TestTicketPromptNamesPotetoSkill(t *testing.T) {
	tempState(t)
	d := Dispatch{Issue: Issue{Key: "FLD-1", Summary: "Thing"}}
	if p := ticketPrompt(d, "/r/one", "rf/fld-1-thing", "the ticket", ""); strings.Contains(p, "poteto") {
		t.Errorf("a plain dispatch mentions poteto-mode:\n%s", p)
	}
	p := ticketPrompt(d, "/r/one", "rf/fld-1-thing", "the ticket", "/plugins/pstack/1.0/skills/poteto-mode/SKILL.md")
	skill, workflow := strings.Index(p, "read `/plugins/pstack/1.0/skills/poteto-mode/SKILL.md`"), strings.Index(p, "## Workflow")
	if skill < 0 || workflow < skill {
		t.Errorf("skill at %d, workflow at %d:\n%s", skill, workflow, p)
	}
}

// The skill's path follows whichever pstack version Claude Code has installed.
func TestPotetoSkillFromInstalledPlugins(t *testing.T) {
	old := installedPlugins
	t.Cleanup(func() { installedPlugins = old })
	dir := t.TempDir()
	installedPlugins = filepath.Join(dir, "installed_plugins.json")
	if _, err := potetoSkill(); err == nil {
		t.Error("no error without installed_plugins.json")
	}
	install := filepath.Join(dir, "pstack", "2.0")
	record := fmt.Sprintf(`{"version":2,"plugins":{"pstack@cursor-plugins":[{"installPath":%q}]}}`, install)
	if err := os.WriteFile(installedPlugins, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := potetoSkill(); err == nil {
		t.Error("no error when the listed install has no skill")
	}
	want := filepath.Join(install, "skills", "poteto-mode", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := potetoSkill(); got != want || err != nil {
		t.Errorf("potetoSkill() = %q, %v; want %q", got, err, want)
	}
}

// skillRepo is a checkout with the given skills; a name ending in ! is one only I can invoke.
func skillRepo(t *testing.T, skills ...string) string {
	t.Helper()
	repo := t.TempDir()
	for _, s := range skills {
		head := "---\nname: " + strings.TrimSuffix(s, "!") + "\n"
		if strings.HasSuffix(s, "!") {
			head += "disable-model-invocation: true\n"
		}
		dir := filepath.Join(repo, ".claude", "skills", strings.TrimSuffix(s, "!"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The body repeats the key: only the frontmatter counts.
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(head+"---\n\ndisable-model-invocation: true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// A repo with Claude Code skills starts on Claude, any other on pi, until I dispatch there with
// another agent.
func TestDefaultAgentFollowsTheRepo(t *testing.T) {
	skilled, plain := skillRepo(t, "fld-watch!"), skillRepo(t)
	s := State{AgentByRepo: map[string]string{}}
	if a := defaultAgent(s, skilled); a != agentClaude {
		t.Errorf("a repo with skills starts on %s", a)
	}
	if a := defaultAgent(s, plain); a != agentPi {
		t.Errorf("a repo without skills starts on %s", a)
	}
	s.AgentByRepo[skilled], s.AgentByRepo[plain] = agentPi, "gone"
	if a := defaultAgent(s, skilled); a != agentPi {
		t.Errorf("the last agent used there lost to %s", a)
	}
	if a := defaultAgent(s, plain); a != agentPi {
		t.Errorf("an agent that no longer exists gave %s", a)
	}
}

// The form's agent follows the repo I move to, until I pick one myself.
func TestFormAgentFollowsTheRepo(t *testing.T) {
	skilled, plain := skillRepo(t, "fld-tdd"), skillRepo(t)
	m := &model{state: State{RepoByProject: map[string]string{}, AgentByRepo: map[string]string{}}, repos: []string{plain, skilled}}
	tab, right, left := tea.KeyPressMsg{Code: tea.KeyTab}, tea.KeyPressMsg{Code: tea.KeyRight}, tea.KeyPressMsg{Code: tea.KeyLeft}
	agent := func() string { return agents[m.form.agent].Label }

	m.openForm(issueRow(Issue{Key: "FLD-1"}))
	if agent() != agentPi {
		t.Fatalf("the form opens on %s for a repo without skills", agent())
	}
	for range fieldRepo {
		m.formInput(tab)
	}
	m.formInput(right)
	if agent() != agentClaude {
		t.Fatalf("on the repo with skills the agent is %s", agent())
	}
	if form := strings.Join(m.renderForm(100, 40), "\n"); !strings.Contains(form, "ships Claude Code skills") {
		t.Errorf("the form doesn't say why:\n%s", form)
	}
	m.formInput(left)
	if agent() != agentPi {
		t.Fatalf("back on the plain repo the agent is %s", agent())
	}

	// Picked by hand, it stays.
	m.openForm(issueRow(Issue{Key: "FLD-1"}))
	m.formInput(right)
	picked := agent()
	for range fieldRepo {
		m.formInput(tab)
	}
	m.formInput(right)
	if agent() != picked {
		t.Errorf("moving repo changed the agent I picked, %s, to %s", picked, agent())
	}

	// A review takes its agent from the PR's checkout.
	m.openForm(prRow(Pr{Key: "o/r#1", Checkout: skilled}))
	if agent() != agentClaude {
		t.Errorf("a review in a repo with skills opens on %s", agent())
	}
}

// Claude is told the repo's skills it can invoke; pi, which can't load them, is not.
func TestTicketPromptNamesRepoSkills(t *testing.T) {
	tempState(t)
	cwd := skillRepo(t, "fld-verify", "fld-tdd", "fld-watch!")
	d := Dispatch{Issue: Issue{Key: "FLD-1", Summary: "Thing"}, Agent: agentClaude}
	p := ticketPrompt(d, cwd, "rf/fld-1-thing", "the ticket", "")
	skills, workflow := strings.Index(p, "its own skills: `fld-tdd`, `fld-verify`."), strings.Index(p, "## Workflow")
	if skills < 0 || workflow < skills || strings.Contains(p, "fld-watch") {
		t.Errorf("skills at %d, workflow at %d:\n%s", skills, workflow, p)
	}
	d.Agent = agentPi
	if p := ticketPrompt(d, cwd, "rf/fld-1-thing", "the ticket", ""); strings.Contains(p, "Repo skills") {
		t.Errorf("pi's prompt names the repo's skills:\n%s", p)
	}
}

// The other tests' queues hold only what they dispatch.
func init() { watchers = nil }

// A watcher is always listed: by its session's state when it runs, as not running otherwise. One
// that waits on a permission prompt needs me, since nothing moves until I answer.
func TestQueueListsWatchers(t *testing.T) {
	t.Cleanup(func() { watchers = nil })
	watchers = []Watcher{{"fld-pr-watch", "/r/watch", "/loop 10m /fld-watch"}}
	s := State{}
	for state, want := range map[string]group{"blocked": needsYou, "working": working, "idle": idle, "done": idle} {
		items := queueItems(s, []rexSession{{ID: "s1", Label: "fld-pr-watch", State: state}}, nil)
		if len(items) != 1 || items[0].Group != want || items[0].Session != "s1" || !items[0].Watcher {
			t.Errorf("%s: %+v", state, items)
		}
	}
	items := queueItems(s, []rexSession{{ID: "s2", Label: "other"}}, nil)
	if len(items) != 1 || items[0].Group != closed || items[0].Agent != "" {
		t.Fatalf("no session: %+v", items)
	}
	if _, label := itemLabel(items[0]); !strings.Contains(label, "not running") {
		t.Errorf("label %q", label)
	}
}

// The watcher runs Claude Code asking before each tool call, whatever my settings default to.
func TestWatcherCommand(t *testing.T) {
	cmd := watcherCommand(Watcher{"w", "/r/watch", "/loop 10m /fld-watch"})
	if got := strings.Join(cmd[1:], " "); got != "--permission-mode manual /loop 10m /fld-watch" {
		t.Errorf("command: %s", got)
	}
}

func TestConfigReplacesWatchers(t *testing.T) {
	oldFile := configFile
	t.Cleanup(func() { configFile, watchers = oldFile, nil })
	configFile = filepath.Join(t.TempDir(), "config.json")
	write := func(body string) error {
		if err := os.WriteFile(configFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadConfig()
	}
	if err := write(`{"watchers": [{"name": "w", "dir": "~/x/y", "prompt": "/loop 5m /z"}]}`); err != nil {
		t.Fatal(err)
	}
	if len(watchers) != 1 || watchers[0].Dir != filepath.Join(home, "x", "y") {
		t.Errorf("watchers %+v", watchers)
	}
	if err := write(`{"watchers": []}`); err != nil || len(watchers) != 0 {
		t.Errorf("an empty list: err %v, watchers %+v", err, watchers)
	}
	if err := write(`{"watchers": [{"name": "w"}]}`); err == nil {
		t.Error("a watcher without a dir and a prompt should be an error")
	}
}

func TestSessionLabel(t *testing.T) {
	for key, want := range map[[2]string]string{
		{"FLD-1671", "My van, always late"}:                 "fld-1671-my-van-always",
		{"FLD-9", ""}:                                       "fld-9",
		{"reports-app#1185", "Anything"}:                    "review-reports-app-1185",
		{"fix:reports-app#1260", "Address review comments"}: "fix-reports-app-1260-address-review-comments",
	} {
		if got := sessionLabel(key[0], key[1]); got != want {
			t.Errorf("sessionLabel(%q, %q) = %q, want %q", key[0], key[1], got, want)
		}
	}
}

// --add-dir takes a list, so --continue must come before it.
func TestResumeCommandContinuesBeforeAddDir(t *testing.T) {
	for _, agent := range []string{agentPi, agentClaude, "Claude (yolo)"} {
		cmd := resumeCommand(agent)
		if cmd[1] != "--continue" {
			t.Errorf("%s: %q should continue right after the binary", agent, cmd)
		}
		if agent != agentPi && cmd[len(cmd)-1] != evidenceRoot {
			t.Errorf("%s: %q should end with the evidence directory", agent, cmd)
		}
	}
}

func TestDispatchedAgent(t *testing.T) {
	oldHome := home
	home = t.TempDir()
	t.Cleanup(func() { home = oldHome })
	s := State{Agents: map[string]string{"FLD-1": "Claude (yolo)"}}
	if got, _ := dispatchedAgent(s, "FLD-1", "/w/x"); got != "Claude (yolo)" {
		t.Errorf("recorded agent: got %q", got)
	}
	if _, err := dispatchedAgent(s, "FLD-2", "/w/repo-fld-2"); err == nil {
		t.Error("no conversation: want an error")
	}
	// Before agents were recorded: the newest conversation's agent.
	dir := "/w/repo.x-fld-2"
	mk := func(p string, at time.Time) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	mk(filepath.Join(home, ".claude", "projects", "-w-repo-x-fld-2"), now.Add(-time.Hour))
	mk(filepath.Join(home, ".pi", "agent", "sessions", "--w-repo.x-fld-2--"), now)
	if got, err := dispatchedAgent(s, "FLD-2", dir); got != agentPi || err != nil {
		t.Errorf("newest is pi: got %q, %v", got, err)
	}
	mk(filepath.Join(home, ".claude", "projects", "-w-repo-x-fld-2"), now.Add(time.Hour))
	if got, _ := dispatchedAgent(s, "FLD-2", dir); got != agentClaude {
		t.Errorf("newest is Claude: got %q", got)
	}
}

// enter on a PR goes to the agent of the ticket its title or branch names.
func TestPrAgent(t *testing.T) {
	q := &queueModel{
		state: State{
			Dispatched: map[string]string{"FLD-1": "/w/1", "FLD-2": "/w/2"},
			Sessions:   map[string]string{"FLD-1": "s1", "FLD-2": "s2"},
			Titles:     map[string]string{"FLD-2": "Two"},
		},
		items: []queueItem{{Kind: agentItem, Key: "FLD-1", Agent: "idle", Session: "s1"}},
	}
	if it, ok := q.prAgent(Pr{Summary: "Fix it", Branch: "rf/fld-1-fix-it"}); !ok || it.Agent != "idle" {
		t.Errorf("by branch: got %+v, %t", it, ok)
	}
	// Dropped from the queue: built from the state, closed.
	if it, ok := q.prAgent(Pr{Summary: "[FLD-2] Two"}); !ok || it.Agent != "" || it.Dir != "/w/2" || it.Title != "Two" {
		t.Errorf("dropped: got %+v, %t", it, ok)
	}
	if _, ok := q.prAgent(Pr{Summary: "FLD-3 not dispatched"}); ok {
		t.Error("an undispatched ticket has no agent")
	}
	if _, ok := q.prAgent(Pr{Summary: "Bump deps"}); ok {
		t.Error("a PR without a key has no agent")
	}
}

// The queue asks the board for a dispatch form; the board takes the request once, and ignores a stale one.
func TestTakeFormRequest(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFile = old })
	issue := Issue{Key: "FLD-7", Summary: "Seven"}
	m := &model{issues: []Row{issueRow(issue)}}

	m.state, _ = updateState(func(s *State) { s.FormRequest = &FormRequest{Key: "FLD-7", At: time.Now()} })
	if cmd := m.takeFormRequest(); cmd != nil {
		t.Error("a loaded ticket needs no fetch")
	}
	if m.form == nil || m.form.row.Key != "FLD-7" {
		t.Fatalf("form = %+v, want FLD-7", m.form)
	}
	if loadState().FormRequest != nil {
		t.Error("the request is still in the state file")
	}

	m.form = nil
	m.state, _ = updateState(func(s *State) { s.FormRequest = &FormRequest{Key: "FLD-7", At: time.Now().Add(-2 * formRequestTTL)} })
	m.takeFormRequest()
	if m.form != nil {
		t.Error("a stale request opened the form")
	}
}

func TestParseDispatchArgs(t *testing.T) {
	a, err := parseDispatchArgs([]string{"--repo", "foreman", "--agent", "pi", "add", "a", "flag"}, nil)
	if err != nil || a.repo != "foreman" || a.agent != "pi" || a.text != "add a flag" {
		t.Errorf("task: %+v, %v", a, err)
	}
	a, err = parseDispatchArgs([]string{"--ticket", "FLD-1", "-"}, strings.NewReader("look at x\n"))
	if err != nil || a.ticket != "FLD-1" || a.text != "look at x" {
		t.Errorf("ticket with stdin: %+v, %v", a, err)
	}
	for _, argv := range [][]string{
		{"do", "it"},                           // a task without a repo
		{"--repo", "foreman"},                  // a task without text
		{"--ticket", "FLD-1", "--pr", "app#1"}, // both
		{"--pr", "app#1", "--repo", "foreman"}, // a review has its checkout
		{"--pr", "app#1", "--title", "t"},      // a review has its PR's title
		{"--fix", "app#1"},                     // a fix is of a PR
		{"--pr", "app#1", "--fix", "--no-worktree"},
		{"--pr", "app#1", "--fix", "--repo", "foreman"},
		{"--ticket", "FLD-1", "--title", "t"}, // a ticket has its summary
		{"--nope"},
	} {
		if _, err := parseDispatchArgs(argv, nil); err == nil {
			t.Errorf("parseDispatchArgs(%q) took it", argv)
		}
	}
}

func TestResolveRepo(t *testing.T) {
	repos := []string{"/w/code/foreman", "/w/code/app", "/p/app"}
	for arg, want := range map[string]string{"foreman": "/w/code/foreman", "Foreman": "/w/code/foreman", "/p/app": "/p/app"} {
		if got, err := resolveRepo(arg, repos); got != want || err != nil {
			t.Errorf("resolveRepo(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	if _, err := resolveRepo("app", repos); err == nil || !strings.Contains(err.Error(), "give its path") {
		t.Errorf("two repos named app: %v", err)
	}
	if _, err := resolveRepo("nope", repos); err == nil {
		t.Error("found a repo that isn't there")
	}
}

func TestGithubName(t *testing.T) {
	if got := githubName("https://github.com/Owner/Reports-App/pull/12", "x"); got != "Reports-App" {
		t.Errorf("githubName = %q", got)
	}
	if got := githubName("", "x"); got != "x" {
		t.Errorf("githubName without a URL = %q", got)
	}
}

// Task numbers count per repo name and aren't reused, so a new task never lands on an old one's
// worktree or report.
func TestNewTaskKey(t *testing.T) {
	tempState(t)
	for _, tc := range []struct{ repo, want string }{
		{"/w/foreman", "task:foreman-1"},
		{"/w/foreman", "task:foreman-2"},
		{"/w/app", "task:app-1"},
	} {
		if key, _, err := newTaskKey(tc.repo); key != tc.want || err != nil {
			t.Errorf("newTaskKey(%s) = %q, %v; want %q", tc.repo, key, err, tc.want)
		}
	}
	discard([]string{"task:foreman-2"})
	if key, n, _ := newTaskKey("/w/foreman"); key != "task:foreman-3" || n != 3 {
		t.Errorf("after cleanup: %q, %d", key, n)
	}
}

// A task's branch is named after its title, numbered when that name is taken here or on origin.
func TestPrepareTaskWorktree(t *testing.T) {
	root := t.TempDir()
	origin, repo := filepath.Join(root, "origin"), filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "develop", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, origin, "branch", "rf/add-a-flag-2")
	git(t, root, "clone", "-q", origin, repo)
	git(t, repo, "branch", "rf/add-a-flag")

	cwd := repo + "-task-1"
	branch, err := prepareTaskWorktree(repo, cwd, "Add a flag", 1, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if branch != "rf/add-a-flag-3" {
		t.Errorf("branch = %q, want rf/add-a-flag-3: -1 is local, -2 on origin", branch)
	}
	if got, want := git(t, cwd, "rev-parse", "HEAD"), git(t, repo, "rev-parse", "origin/develop"); got != want {
		t.Errorf("the task starts at %s, want origin/develop %s", got, want)
	}
	if _, err := prepareTaskWorktree(repo, cwd, "Add a flag", 1, func(string) {}); err == nil {
		t.Error("reused an existing directory")
	}
}

func TestTaskPrompt(t *testing.T) {
	tempState(t)
	p := taskPrompt("task:foreman-1", "Add a flag", "Add a flag\n\nto the CLI", agentPi, "/w/foreman-task-1", "rf/add-a-flag", true, "")
	for _, want := range []string{
		`# task:foreman-1: "Add a flag"`,
		"there is no Jira ticket",
		"## Task\n\nAdd a flag\n\nto the CLI",
		"answer it in the report and leave the code as it is",
		"Do not push",
		evidenceDir("task:foreman-1") + "/report.md",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "## Method") {
		t.Error("names poteto-mode without being asked")
	}
}

func TestTaskSession(t *testing.T) {
	label := sessionLabel("task:my-long-repo-name-here-1", "Add a flag to it")
	if label != "task-my-long-repo-name-here-1-add-a-flag" {
		t.Errorf("sessionLabel = %q", label)
	}
	sessions := []rexSession{{ID: "a", Label: "task-my-long-repo-name-here-10-other"}, {ID: "b", Label: label}}
	if s, ok := findSession(sessions, "task:my-long-repo-name-here-1", ""); !ok || s.ID != "b" {
		t.Errorf("findSession = %+v, %t", s, ok)
	}
}

// A task has nothing outside to say it's done: it is a cleanup candidate once its session closes.
func TestCleanupListsClosedTasks(t *testing.T) {
	root := t.TempDir()
	repo, worktree := filepath.Join(root, "repo"), filepath.Join(root, "repo-task-1")
	git(t, root, "init", "-q", "-b", "main", repo)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, repo, "worktree", "add", "-q", worktree)

	if _, ok, err := checkCleanup(context.Background(), nil, "task:repo-1", worktree, true); ok || err != nil {
		t.Errorf("open session: candidate %t, %v", ok, err)
	}
	c, ok, err := checkCleanup(context.Background(), nil, "task:repo-1", worktree, false)
	if !ok || err != nil || c.Reason != "session closed" || c.Main || c.Dirty {
		t.Errorf("closed session: %+v, %t, %v", c, ok, err)
	}
}

// ^t opens a task form on the context field; it won't dispatch until the task says something.
func TestTaskForm(t *testing.T) {
	tempState(t)
	m := &model{
		state:    State{RepoByProject: map[string]string{"FLD": "/r/two"}, AgentByRepo: map[string]string{}},
		repos:    []string{"/r/one", "/r/two"},
		issues:   []Row{issueRow(Issue{Key: "FLD-1"})},
		details:  map[string][]string{},
		detailOK: map[string]bool{},
		agents:   map[string]string{},
	}
	m.boardInput(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	f := m.form
	if f == nil || !f.task || f.field != fieldContext || m.formRepo() != "/r/two" {
		t.Fatalf("form = %+v, repo %s", f, m.formRepo())
	}
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	if cmd := m.formInput(enter); cmd != nil || f.busy || !strings.Contains(m.status, "Describe the task") {
		t.Fatalf("empty task: cmd %t, busy %t, status %q", cmd != nil, f.busy, m.status)
	}
	m.paste("Add a flag")
	if form := strings.Join(m.renderForm(100, 40), "\n"); !strings.Contains(form, "New task") || !strings.Contains(form, "Add a flag") {
		t.Errorf("form:\n%s", form)
	}
	if cmd := m.formInput(enter); cmd == nil || !f.busy {
		t.Fatal("the task didn't dispatch")
	}
	// The agent is remembered for the repo; the project's repo stays the ticket dispatches' choice.
	if s := loadState(); len(s.RepoByProject) != 0 || s.AgentByRepo["/r/two"] == "" {
		t.Errorf("state = %+v", s)
	}
}

// wait returns once a working agent stops; one that isn't working gets the settle time to start,
// so a reply sent just before isn't missed.
func TestWaitIdle(t *testing.T) {
	run := func(states ...string) (queueItem, time.Duration, error) {
		n := 0
		look := func() (queueItem, error) {
			it := queueItem{Key: "FLD-1", Agent: states[min(n, len(states)-1)]}
			n++
			return it, nil
		}
		var slept time.Duration
		it, err := waitIdle(look, time.Minute, 10*time.Second, 2*time.Second, func(d time.Duration) { slept += d })
		return it, slept, err
	}
	if it, slept, err := run("done", "working", "working", "blocked"); err != nil || it.Agent != "blocked" || slept != 6*time.Second {
		t.Errorf("a reply that starts work: %q after %s, %v", it.Agent, slept, err)
	}
	if it, slept, err := run("idle"); err != nil || it.Agent != "idle" || slept != 10*time.Second {
		t.Errorf("an agent that never starts: %q after %s, %v", it.Agent, slept, err)
	}
	if it, _, err := run("working", ""); err != nil || it.Agent != "" {
		t.Errorf("a session that closes: %q, %v", it.Agent, err)
	}
	if _, slept, err := run("working"); err == nil || !strings.Contains(err.Error(), "still working after 1m0s") || slept != time.Minute {
		t.Errorf("an agent that keeps working: after %s, %v", slept, err)
	}
}

func TestReplyAllowed(t *testing.T) {
	for _, tc := range []struct {
		agent string
		now   bool
		ok    bool
	}{
		{"blocked", false, true},
		{"done", false, true},
		{"working", false, false},
		{"working", true, true},
		{"", true, false},
	} {
		if err := replyAllowed(queueItem{Key: "FLD-1", Agent: tc.agent}, tc.now, byMe); (err == nil) != tc.ok {
			t.Errorf("reply to %q, now %t: %v", tc.agent, tc.now, err)
		}
	}
}

// A seen token covers the agent's state and its pane's last lines, not output further up.
func TestSeenToken(t *testing.T) {
	var older []string
	for n := range 30 {
		older = append(older, fmt.Sprintf("output %d", n))
	}
	prompt := "Bash command\n  rm -rf dist\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n\nEsc to cancel\n"
	screen := strings.Join(older, "\n") + "\n" + prompt
	token := seenToken("blocked", screen)
	if !strings.HasPrefix(token, "blocked-") || len(token) != len("blocked-")+8 {
		t.Errorf("token = %q", token)
	}
	for name, same := range map[string]string{
		"a clock further up":  strings.Replace(screen, "output 2\n", "output 2 12:31\n", 1),
		"padding":             strings.ReplaceAll(screen, "\n", "  \n") + "\n\n",
		"blank lines between": strings.Replace(screen, "Esc to cancel", "\n\nEsc to cancel", 1),
	} {
		if got := seenToken("blocked", same); got != token {
			t.Errorf("%s changed the token: %s, was %s", name, got, token)
		}
	}
	answered := strings.Join(older, "\n") + "\n⏺ Bash(rm -rf dist)\n✻ Working… (2s)\n❯ \n"
	for name, other := range map[string]string{
		"the prompt answered": seenToken("blocked", answered),
		"the state":           seenToken("idle", screen),
	} {
		if other == token {
			t.Errorf("%s left the token as it was", name)
		}
	}
	if got := seenToken("", ""); got != seenToken("", "\n \n") || !strings.HasPrefix(got, "closed-") {
		t.Errorf("an empty pane: %q", got)
	}
}

// reply --seen types only while the pane shows what the token was made from: a permission prompt
// answered in the meantime refuses the reply, rather than typing "1" as a message.
func TestReplyRefusesAfterTheScreenChanged(t *testing.T) {
	it := queueItem{Kind: agentItem, Key: "reports-app#1260", Agent: "blocked", Session: "s1"}
	prompt := "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	screen := prompt
	capture := func(got queueItem) (string, error) {
		if got.Session != "s1" {
			t.Errorf("captured session %q", got.Session)
		}
		return screen, nil
	}
	infos := []agentInfo{{Key: it.Key}, {Key: "closed"}}
	addSeen(infos, []queueItem{it, {Kind: agentItem, Key: "closed"}}, capture)
	seen := infos[0].Seen
	if seen != seenToken("blocked", prompt) || infos[1].Seen != "" {
		t.Fatalf("agents --json tokens: %+v", infos)
	}
	if err := stillSeen(it, seen, capture); err != nil {
		t.Errorf("unchanged pane: %v", err)
	}

	screen = "⏺ Bash(npm test)\n✻ Working…\n❯ \n"
	err := stillSeen(it, seen, capture)
	if err == nil || !strings.Contains(err.Error(), "foreman screen reports-app#1260 again") || !strings.Contains(err.Error(), "its pane changed") {
		t.Errorf("answered prompt: %v", err)
	}

	screen, it.Agent = prompt, "idle"
	if err := stillSeen(it, seen, capture); err == nil || !strings.Contains(err.Error(), "it was blocked and is idle now") {
		t.Errorf("state changed: %v", err)
	}

	failing := func(queueItem) (string, error) { return "", errors.New("no session s1") }
	if err := stillSeen(it, seen, failing); err == nil {
		t.Error("sent without reading the pane")
	}
}

// The agents command reports what the queue shows, by key in any case, watchers included.
func TestAgentsCommand(t *testing.T) {
	tempState(t)
	dispatched(t, "task:foreman-1", "/w/foreman-task-1")
	dispatched(t, "FLD-2", "/w/app-fld-2")
	_, _ = updateState(func(s *State) {
		s.Sessions["task:foreman-1"], s.Agents["task:foreman-1"] = "s1", agentPi
		s.Titles["task:foreman-1"] = "Add a flag"
	})
	old := watchers
	watchers = []Watcher{{Name: "fld-pr-watch", Dir: "/w/watch", Prompt: "/loop"}}
	t.Cleanup(func() { watchers = old })
	s := loadState()
	items := queueItems(s, []rexSession{{ID: "s1", Label: "task-foreman-1-add-a-flag", State: "blocked"}}, nil)

	it, err := findItem(items, "TASK:Foreman-1")
	if err != nil || it.Agent != "blocked" {
		t.Fatalf("findItem = %+v, %v", it, err)
	}
	if _, err := findItem(items, "fld-pr-watch"); err != nil {
		t.Errorf("watcher: %v", err)
	}
	if _, err := findItem(items, "FLD-9"); err == nil {
		t.Error("found an agent that was never dispatched")
	}

	a := agentInfoOf(it, s)
	if a.Kind != "task" || a.Group != "needs you" || a.State != "blocked" || a.Agent != agentPi || a.ReportAt == nil || a.Title != "Add a flag" {
		t.Errorf("agentInfo = %+v", a)
	}
	var infos []agentInfo
	for _, it := range items {
		infos = append(infos, agentInfoOf(it, s))
	}
	var out strings.Builder
	if err := writeAgents(&out, infos); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"needs you  task:foreman-1  blocked   Add a flag  report", "review     FLD-2           closed", "closed     fld-pr-watch    closed    /loop"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("agents output lacks %q:\n%s", want, out.String())
		}
	}
}

// cleanup with no keys takes what ^x ticks; with keys, only those, and only when they're ready.
func TestPickCandidates(t *testing.T) {
	all := []cleanupCandidate{
		{Key: "FLD-1"},
		{Key: "FLD-2", Dirty: true},
		{Key: "task:app-1", Agent: "working"},
	}
	if got, _ := pickCandidates(all, nil); len(got) != 1 || got[0].Key != "FLD-1" {
		t.Errorf("default = %+v", got)
	}
	if got, err := pickCandidates(all, []string{"fld-2"}); err != nil || len(got) != 1 || got[0].Key != "FLD-2" {
		t.Errorf("by key = %+v, %v", got, err)
	}
	if _, err := pickCandidates(all, []string{"FLD-9"}); err == nil {
		t.Error("picked a dispatch that isn't a candidate")
	}
}

func TestCommandArgs(t *testing.T) {
	if _, err := parseFlags(newFlags("report"), nil, 1); err == nil || !strings.Contains(err.Error(), "takes 1 argument") {
		t.Errorf("report without a key: %v", err)
	}
	// Options go before or after the key.
	for _, argv := range [][]string{{"--lines", "0", "FLD-1"}, {"FLD-1", "--lines", "0"}} {
		fs := newFlags("screen")
		lines := fs.Int("lines", 40, "")
		if keys, err := parseFlags(fs, argv, 1); err != nil || *lines != 0 || !slices.Equal(keys, []string{"FLD-1"}) {
			t.Errorf("screen %q: %q, %v, lines %d", argv, keys, err, *lines)
		}
	}
	fs := newFlags("cleanup")
	dry := fs.Bool("dry-run", false, "")
	if keys, err := parseFlags(fs, []string{"FLD-1", "--dry-run", "FLD-2"}, -1); err != nil || !*dry || len(keys) != 2 {
		t.Errorf("cleanup: %q, %v, dry %t", keys, err, *dry)
	}
}

// The orchestrator is listed while it runs, with the agents that need me when it waits on a
// permission prompt.
func TestOrchestratorItem(t *testing.T) {
	if _, ok := orchestratorItem(nil, ""); ok {
		t.Error("listed an orchestrator that isn't running")
	}
	// A session whose label only starts with the name is someone else's.
	if _, ok := orchestratorItem([]rexSession{{ID: "x", Label: orchestratorName + "-chat"}}, ""); ok {
		t.Error("took foreman-orchestrator-chat for the orchestrator")
	}
	if it, ok := orchestratorItem([]rexSession{{ID: "o2", Label: "renamed-it"}}, "o2"); !ok || it.Session != "o2" {
		t.Errorf("a renamed orchestrator: %+v, %t", it, ok)
	}
	for state, want := range map[string]group{"blocked": needsYou, "working": working, "idle": idle, "open": idle} {
		it, ok := orchestratorItem([]rexSession{{ID: "o1", Label: orchestratorName, State: state}}, "")
		if !ok || it.Group != want || it.Session != "o1" || kindName(it) != "orchestrator" {
			t.Errorf("%s: %+v, %t", state, it, ok)
		}
	}
}

// Read-only foreman commands run unasked; --allowedTools takes a list, so it comes last.
func TestOrchestratorCommand(t *testing.T) {
	cmd := orchestratorCommand(true)
	at := slices.Index(cmd, "--allowedTools")
	mode := slices.Index(cmd, "--permission-mode")
	if mode < 0 || cmd[mode+1] != "manual" {
		t.Errorf("the orchestrator follows my default permission mode: %q", cmd)
	}
	if !slices.Contains(cmd, "--continue") || at < 0 || !slices.Equal(cmd[at+1:], orchestratorTools) {
		t.Errorf("command = %q", cmd)
	}
	if slices.Contains(orchestratorCommand(false), "--continue") {
		t.Error("a new conversation continues the last one")
	}
	for _, tool := range orchestratorTools {
		if strings.Contains(tool, "dispatch") {
			t.Errorf("%s runs without asking", tool)
		}
	}
	for _, tool := range []string{"Bash(foreman reply:*)", "Bash(foreman cleanup:*)"} {
		if !slices.Contains(orchestratorTools, tool) {
			t.Errorf("%s asks each time", tool)
		}
	}
	if err := runOrchestrator([]string{"--old"}); err == nil {
		t.Error("took an unknown option")
	}
}

func TestAskerKey(t *testing.T) {
	s := State{
		Sessions:   map[string]string{"FLD-1": "session:a"},
		Dispatched: map[string]string{"FLD-1": "/w/app-fld-1", "task:app-2": "/w/app-task-2"},
	}
	for _, tc := range []struct{ session, cwd, want string }{
		{"session:a", "/elsewhere", "FLD-1"},
		{"", "/w/app-task-2/src/lib", "task:app-2"},
		{"session:z", "/w/app-task-2", "task:app-2"},
	} {
		if got, err := askerKey(s, tc.session, tc.cwd); got != tc.want || err != nil {
			t.Errorf("askerKey(%q, %q) = %q, %v; want %q", tc.session, tc.cwd, got, err, tc.want)
		}
	}
	// A sibling directory sharing the worktree's prefix isn't in it.
	if _, err := askerKey(s, "", "/w/app-task-22"); err == nil {
		t.Error("took /w/app-task-22 for /w/app-task-2")
	}
}

// ask waits inside the agent's command and prints the answer; the log keeps both.
func TestAskAnswered(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	notified := ""
	notify := func(key, text string) error { notified = key + ": " + text; return nil }
	sleep := func(time.Duration) {
		if q := loadState().Questions["FLD-1"]; q.Escalated != "" {
			t.Error("a question the orchestrator heard waits on me")
		}
		if err := answerQuestion("FLD-1", "Use the v2 endpoint", byOrchestrator); err != nil {
			t.Error(err)
		}
	}
	answer, err := ask("FLD-1", "Which endpoint?", time.Minute, time.Second, sleep, notify)
	if err != nil || answer != "Use the v2 endpoint" || notified != "FLD-1: Which endpoint?" {
		t.Fatalf("ask = %q, %v; notified %q", answer, err, notified)
	}
	if _, ok := loadState().Questions["FLD-1"]; ok {
		t.Error("the answered question is still pending")
	}
	var kinds []string
	for _, e := range readLog("FLD-1") {
		kinds = append(kinds, e.By+" "+e.Kind)
	}
	if !slices.Equal(kinds, []string{"agent question", "orchestrator answer"}) {
		t.Errorf("log = %q", kinds)
	}
}

// With no orchestrator to hear it, the question waits on me; unanswered, ask gives up and says so.
func TestAskWithoutOrchestrator(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	gone := func(string, string) error { return errors.New("it isn't running") }
	var escalated string
	sleep := func(time.Duration) { escalated = loadState().Questions["FLD-1"].Escalated }
	_, err := ask("FLD-1", "Which endpoint?", 3*time.Second, time.Second, sleep, gone)
	if err == nil || !strings.Contains(err.Error(), "stop to ask me") || !strings.Contains(escalated, "isn't running") {
		t.Fatalf("err %v, escalated %q", err, escalated)
	}
	if _, ok := loadState().Questions["FLD-1"]; ok {
		t.Error("an expired question is still pending: later replies would answer nobody")
	}
	if log := readLog("FLD-1"); len(log) != 2 || log[1].Kind != "expired" {
		t.Errorf("log = %+v", log)
	}
}

// Dispatching the agent again withdraws its question.
func TestAskWithdrawn(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	sleep := func(time.Duration) { markDispatched("FLD-1", "t", "/w/app-fld-1", "s2", agentPi) }
	if _, err := ask("FLD-1", "?", time.Minute, time.Second, sleep, func(string, string) error { return nil }); err == nil || !strings.Contains(err.Error(), "withdrawn") {
		t.Errorf("err = %v", err)
	}
}

// The orchestrator answers or messages a dispatch a few times at most; I have no limit.
func TestOrchestratorLimits(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	pend := func() {
		_, _ = updateState(func(s *State) { s.Questions["FLD-1"] = Question{Text: "?", At: time.Now()} })
	}
	for n := 0; n < maxOrchestratorAnswers; n++ {
		pend()
		if err := answerQuestion("FLD-1", "yes", byOrchestrator); err != nil {
			t.Fatalf("answer %d: %v", n+1, err)
		}
	}
	pend()
	if err := answerQuestion("FLD-1", "yes", byOrchestrator); err == nil || !strings.Contains(err.Error(), "escalate") {
		t.Errorf("past the limit: %v", err)
	}
	if err := replyAllowed(queueItem{Key: "FLD-1", Agent: "done"}, false, byOrchestrator); err == nil {
		t.Error("the orchestrator messaged past the limit")
	}
	if err := answerQuestion("FLD-1", "yes", byMe); err != nil {
		t.Errorf("my answer: %v", err)
	}
	if err := answerQuestion("FLD-1", "again", byMe); err == nil {
		t.Error("answered a question that was already answered")
	}

	tempState(t)
	dispatched(t, "FLD-2", "/w/app-fld-2")
	if err := replyAllowed(queueItem{Key: "FLD-2", Agent: "blocked"}, false, byOrchestrator); err == nil || !strings.Contains(err.Error(), "permission prompt") {
		t.Errorf("the orchestrator typed into a blocked agent: %v", err)
	}
	if err := replyAllowed(queueItem{Key: "FLD-2", Agent: "blocked"}, false, byMe); err != nil {
		t.Errorf("my reply to a blocked agent: %v", err)
	}
	if err := escalateQuestion("FLD-2", "use v2"); err == nil {
		t.Error("escalated a question nobody asked")
	}
}

// The queue lists an agent that asks the orchestrator as working, and one whose question came to me
// with what needs me, until I mark it.
func TestQueueShowsQuestions(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	at := time.Now()
	_, _ = updateState(func(s *State) {
		s.Sessions["FLD-1"] = "s1"
		s.Questions["FLD-1"] = Question{Text: "Which endpoint?", At: at}
	})
	_ = appendLog("FLD-1", logEntry{At: at, By: byAgent, Kind: "question", Text: "Which endpoint?"})
	sessions := []rexSession{{ID: "s1", Label: "fld-1", State: "working"}}
	it := queueItems(loadState(), sessions, nil)[0]
	if _, label := itemLabel(it); it.Group != working || !strings.Contains(label, "asks the orchestrator") {
		t.Errorf("pending: group %d, label %q", it.Group, label)
	}
	_ = escalateQuestion("FLD-1", "use v2")
	it = queueItems(loadState(), sessions, nil)[0]
	if _, label := itemLabel(it); it.Group != needsYou || !strings.Contains(label, "asks you") {
		t.Errorf("escalated: group %d, label %q", it.Group, label)
	}
	detail := strings.Join(itemDetail(it), "\n")
	for _, want := range []string{"Which endpoint?", "suggests:** use v2", "**agent asks**: Which endpoint?"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	q := &queueModel{state: loadState(), items: []queueItem{it}, details: map[string][]string{}}
	q.input(tea.KeyPressMsg{Code: 'f', Text: "f"})
	if q.feedback == nil || !q.feedback.answer {
		t.Fatalf("f on a question: %+v", q.feedback)
	}
}

func TestAgentsAsk(t *testing.T) {
	tempState(t)
	p := taskPrompt("task:app-1", "t", "do it", agentClaude, "/w", "b", true, "")
	if !strings.Contains(p, "## Asking") || !strings.Contains(p, "`foreman ask") {
		t.Errorf("the task prompt doesn't say how to ask:\n%s", p)
	}
	cmd := agentArgs(agentClaude)
	at, add := slices.Index(cmd, "--allowedTools"), slices.Index(cmd, "--add-dir")
	if at < 0 || cmd[at+1] != "Bash(foreman ask:*)" || add != at+2 {
		t.Errorf("claude's args = %q", cmd)
	}
	if strings.Contains(orchestratorGuide, "§") || !strings.Contains(orchestratorGuide, "`foreman escalate KEY") {
		t.Errorf("guide:\n%s", orchestratorGuide)
	}
	if !slices.Contains(orchestratorTools, "Bash(foreman answer:*)") || !slices.Contains(orchestratorTools, "Bash(foreman escalate:*)") {
		t.Errorf("the orchestrator would ask before answering: %q", orchestratorTools)
	}
}

// A watcher that renamed its session is still found, by its recorded ID or by Claude Code running
// in its checkout, so the board doesn't start a second one.
func TestFindWatcherAfterARename(t *testing.T) {
	w := Watcher{Name: "fld-pr-watch", Dir: "/w/watch", Prompt: "/loop"}
	renamed := rexSession{ID: "s1", Label: "pr-review-watcher", State: "idle",
		Procs: []rexProc{{Cmd: "/Users/me/.local/bin/claude", Dir: "/w/watch"}}}
	if s, ok := findWatcher([]rexSession{renamed}, w, ""); !ok || s.ID != "s1" {
		t.Errorf("by its checkout: %+v, %t", s, ok)
	}
	byID := rexSession{ID: "s2", Label: "anything"}
	if s, ok := findWatcher([]rexSession{byID}, w, "s2"); !ok || s.ID != "s2" {
		t.Errorf("by its recorded ID: %+v, %t", s, ok)
	}
	for _, other := range []rexSession{
		{ID: "s3", Label: "shell", Procs: []rexProc{{Cmd: "/bin/zsh", Dir: "/w/watch"}}},
		{ID: "s4", Label: "x", Procs: []rexProc{{Cmd: "/bin/claude", Dir: "/w/watch-2"}}},
	} {
		if _, ok := findWatcher([]rexSession{other}, w, ""); ok {
			t.Errorf("took %+v for the watcher", other)
		}
	}
	if it := watcherItem(w, []rexSession{renamed}, ""); it.Group != idle || it.Session != "s1" {
		t.Errorf("queue row: %+v", it)
	}
	o := rexSession{ID: "o1", Label: "renamed", Procs: []rexProc{{Cmd: "claude", Dir: orchestratorDir}}}
	if s, ok := findOrchestrator([]rexSession{o}, ""); !ok || s.ID != "o1" {
		t.Errorf("orchestrator by its directory: %+v, %t", s, ok)
	}
}

func TestParseDispatchArgsFix(t *testing.T) {
	a, err := parseDispatchArgs([]string{"--pr", "app#1", "--fix", "--title", "Comments", "--agent", "pi", "the", "nits"}, nil)
	if err != nil || !a.fix || a.pr != "app#1" || a.title != "Comments" || a.text != "the nits" {
		t.Errorf("fix: %+v, %v", a, err)
	}
	if a, err := parseDispatchArgs([]string{"--pr", "app#1"}, nil); err != nil || a.fix {
		t.Errorf("review: %+v, %v", a, err)
	}
}

// fixRepo is a clone of an origin with the PR branch feature/x, which gets a commit after the clone.
func fixRepo(t *testing.T) (root, repo, head string) {
	root = t.TempDir()
	origin := filepath.Join(root, "origin")
	repo = filepath.Join(root, "repo")
	git(t, root, "init", "-q", "-b", "develop", origin)
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, origin, "checkout", "-q", "-b", "feature/x")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "pr work")
	git(t, origin, "checkout", "-q", "develop")
	git(t, root, "clone", "-q", origin, repo)
	pushed := git(t, origin, "commit-tree", "feature/x^{tree}", "-p", "feature/x", "-m", "pushed after the clone")
	git(t, origin, "update-ref", "refs/heads/feature/x", pushed)
	return root, repo, git(t, origin, "rev-parse", "feature/x")
}

// A PR branch nobody has checked out gets a new worktree on a local branch tracking origin's, at
// origin's newest commit.
func TestPrepareFixWorktreeNewTrackingBranch(t *testing.T) {
	_, repo, head := fixRepo(t)
	cwd := repo + "-fix-7"
	dir, reused, err := prepareFixWorktree(repo, cwd, "feature/x", "develop", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if dir != cwd || reused {
		t.Errorf("got %s, reused %t; want a new %s", dir, reused, cwd)
	}
	if got := git(t, cwd, "branch", "--show-current"); got != "feature/x" {
		t.Errorf("branch = %q, want feature/x", got)
	}
	if got := git(t, cwd, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/feature/x" {
		t.Errorf("upstream = %q", got)
	}
	if got := git(t, cwd, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want origin's current %s", got, head)
	}
}

// A PR branch checked out in a worktree, say the ticket's, is fixed there, whatever it holds, and
// gets origin's branch as its upstream when it has none.
func TestPrepareFixWorktreeReusesTheBranchsWorktree(t *testing.T) {
	root, repo, _ := fixRepo(t)
	byHand := filepath.Join(root, "repo-fld-1786")
	git(t, repo, "worktree", "add", "-q", "--no-track", "-b", "feature/x", byHand, "origin/feature/x")
	if err := os.WriteFile(filepath.Join(byHand, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, reused, err := prepareFixWorktree(repo, repo+"-fix-7", "feature/x", "develop", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(byHand)
	if got, _ := filepath.EvalSymlinks(dir); got != want || !reused {
		t.Errorf("got %s, reused %t; want %s", dir, reused, byHand)
	}
	if exists(repo + "-fix-7") {
		t.Error("created a second worktree")
	}
	if !exists(filepath.Join(byHand, "wip.txt")) {
		t.Error("the reused worktree lost its uncommitted file")
	}
	if got := git(t, byHand, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/feature/x" {
		t.Errorf("upstream = %q", got)
	}
}

// A local PR branch that isn't checked out is used as is: it may hold unpushed commits.
func TestPrepareFixWorktreeKeepsLocalCommits(t *testing.T) {
	_, repo, _ := fixRepo(t)
	git(t, repo, "branch", "--no-track", "feature/x", "origin/feature/x")
	git(t, repo, "checkout", "-q", "feature/x")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "unpushed")
	unpushed := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-q", "develop")

	cwd := repo + "-fix-7"
	if _, _, err := prepareFixWorktree(repo, cwd, "feature/x", "develop", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got := git(t, cwd, "rev-parse", "HEAD"); got != unpushed {
		t.Errorf("HEAD = %s, want the unpushed %s", got, unpushed)
	}
	if got := git(t, cwd, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/feature/x" {
		t.Errorf("upstream = %q", got)
	}
}

func TestPrepareFixWorktreeRefusesAnotherCheckoutInItsPlace(t *testing.T) {
	_, repo, _ := fixRepo(t)
	cwd := repo + "-fix-7"
	git(t, repo, "worktree", "add", "-q", "--detach", cwd, "origin/develop")
	if _, _, err := prepareFixWorktree(repo, cwd, "feature/x", "develop", func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't on feature/x") {
		t.Errorf("err = %v", err)
	}
}

func testDiscussion(t *testing.T) PrDiscussion {
	var d PrDiscussion
	err := json.Unmarshal([]byte(`{
	  "reviews": {"totalCount": 3, "nodes": [
	    {"author": {"login": "ana"}, "state": "CHANGES_REQUESTED", "body": "Needs tests.\n\n## Also\nrename it", "submittedAt": "2026-11-10"},
	    {"author": {"login": "ana"}, "state": "COMMENTED", "body": "", "submittedAt": "2026-11-10"},
	    {"author": {"login": "bo"}, "state": "APPROVED", "body": "", "submittedAt": "2026-11-11"}
	  ]},
	  "comments": {"totalCount": 1, "nodes": [{"author": null, "body": "ping", "createdAt": "2026-11-11"}]},
	  "reviewThreads": {"totalCount": 3, "nodes": [
	    {"isResolved": false, "isOutdated": false, "path": "src/a.ts", "line": 12,
	     "comments": {"totalCount": 2, "nodes": [{"author": {"login": "ana"}, "body": "Off by one", "createdAt": "d1"}, {"author": {"login": "li"}, "body": "Why?", "createdAt": "d2"}]}},
	    {"isResolved": false, "isOutdated": true, "path": "src/b.ts", "line": 0, "originalLine": 4,
	     "comments": {"totalCount": 1, "nodes": [{"author": {"login": "ana"}, "body": "Drop this", "createdAt": "d3"}]}},
	    {"isResolved": true, "path": "src/c.ts", "line": 1,
	     "comments": {"totalCount": 1, "nodes": [{"author": {"login": "ana"}, "body": "Done already", "createdAt": "d4"}]}}
	  ]}
	}`), &d)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDiscussionMarkdown(t *testing.T) {
	md := discussionMarkdown(testDiscussion(t))
	for _, want := range []string{
		"**ana** (2026-11-10), changes requested:\n\n> Needs tests.\n>\n> ## Also\n> rename it",
		"### `src/a.ts:12`\n\n**ana** (d1):\n\n> Off by one\n\n**li** (d2):\n\n> Why?",
		"### `src/b.ts:4` (outdated: the code moved since)",
		"_1 resolved threads left out._",
		"## Conversation\n\n**ghost** (2026-11-11):\n\n> ping",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("lacks %q:\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"Done already", "bo", "commented"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("shows %q:\n%s", unwanted, md)
		}
	}
	if md := discussionMarkdown(PrDiscussion{}); !strings.Contains(md, "## Unresolved review threads\n\n_None._") {
		t.Errorf("an empty discussion:\n%s", md)
	}
}

func TestFixPrompt(t *testing.T) {
	tempState(t)
	pr := Pr{Key: "reports-app#1260", Repo: "bloqit/reports-app", Number: 1260}
	v := PrView{Title: "Van filter", HeadRefName: "li/FLD-1786-van-filter", BaseRefName: "develop"}
	v.Author.Login = "li"
	d := PrDispatch{Pr: pr, Agent: agentPi, Fix: true}
	md := discussionMarkdown(testDiscussion(t))
	p := fixPrompt(d, v, md, "FLD-1786", "the ticket", "/w/reports-app-fld-1786", true)
	for _, want := range []string{
		`# Fix bloqit/reports-app#1260: "Van filter"`,
		"Working directory: /w/reports-app-fld-1786",
		"Branch: li/FLD-1786-van-filter, tracking origin/li/FLD-1786-van-filter (a worktree that already had it checked out",
		"`git diff origin/develop...HEAD`",
		"## Unresolved review threads",
		"Off by one",
		"## Jira ticket\n\nthe ticket",
		"Address each unresolved review thread",
		"do not push, reply to or resolve comments",
		"foreman ask",
		evidenceDir("fix:reports-app#1260") + "/report.md",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Do not change it") || strings.Contains(p, "## What to fix") {
		t.Errorf("a fix without context reads as a review or names a task:\n%s", p)
	}

	d.Context = "Only the naming nits"
	p = fixPrompt(d, v, md, "", "", "/w/reports-app-fix-1260", false)
	for _, want := range []string{"## What to fix\n\nOnly the naming nits", "a git worktree created for this fix", "Do what the What to fix section asks"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, p)
		}
	}
}

// A fix's key keeps it apart from a review of the same PR: its evidence, its kind, and the PR's
// queue row leads to it.
func TestFixKey(t *testing.T) {
	if evidenceDir("fix:reports-app#1260") == evidenceDir("reports-app#1260") {
		t.Error("a fix and a review share an evidence directory")
	}
	if got := kindName(queueItem{Kind: agentItem, Key: "fix:reports-app#1260"}); got != "fix" {
		t.Errorf("kind = %q", got)
	}
	if got := kindName(queueItem{Kind: agentItem, Key: "reports-app#1260"}); got != "review" {
		t.Errorf("a review's kind = %q", got)
	}
	q := &queueModel{state: State{
		Dispatched: map[string]string{"FLD-1": "/w/1", "fix:app#5": "/w/1"},
		Sessions:   map[string]string{"FLD-1": "s1", "fix:app#5": "s2"},
	}}
	if it, ok := q.prAgent(Pr{Key: "app#5", Branch: "rf/fld-1-x"}); !ok || it.Key != "fix:app#5" {
		t.Errorf("prAgent = %+v, %t; want the fix", it, ok)
	}
	m := &model{state: State{Dispatched: map[string]string{"fix:app#5": "/w/1"}}}
	if got := m.agentKey("app#5"); got != "fix:app#5" {
		t.Errorf("agentKey = %q", got)
	}
	m.state.Dispatched["app#5"] = "/w/app-pr-5"
	if got := m.agentKey("app#5"); got != "app#5" {
		t.Errorf("agentKey with a review = %q", got)
	}
}
