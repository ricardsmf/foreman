package main

import (
	"context"
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
	branch, err := prepareWorktree(repo, cwd, Issue{Key: "FLD-7", Summary: "Thing"}, func(string) {})
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
	branch, err := prepareWorktree(repo, cwd, Issue{Key: "FLD-8", Summary: "New thing"}, func(string) {})
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
	}
	// FLD-2 asks, FLD-3 works, FLD-5 finished without a report, FLD-4's session is closed.
	sessions := []rexSession{{"s1", "fld-1-a", "done"}, {"s2", "fld-2-b", "blocked"}, {"s3", "fld-3-c", "working"}, {"s7", "review-repo-7", "idle"}, {"s5", "fld-5-e", "done"}}
	prs := []Pr{
		{Key: "repo#9", Mine: true, Category: "Blocked", Status: "in review ✗"},
		{Key: "repo#10", Mine: true, Category: "Done"},
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
	want := map[string]group{"FLD-2": needsYou, "repo#9": needsYou, "FLD-1": toReview, "FLD-4": toReview, "repo#7": toReview, "FLD-3": working, "FLD-5": idle}
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
	s.Dismissed["repo#9"] = "in review ✗"
	if _, ok := groups(queueItems(s, sessions, prs))["repo#9"]; ok {
		t.Error("reviewed repo#9 still listed")
	}
}

func TestFindSession(t *testing.T) {
	sessions := []rexSession{
		{"session:a", "fld-1704", "done"},
		{"session:b", "review-reports-app-1185-x", "idle"},
		{"session:c", "fld-17040-other", "working"},
	}
	for _, tc := range []struct{ key, recorded, want string }{
		{"FLD-1704", "session:c", "session:c"},                 // the recorded ID wins
		{"FLD-1704", "fld-1704-test-work-orders", "session:a"}, // renamed: matched by the key
		{"reports-app#1185", "gone", "session:b"},
		{"FLD-170", "", ""}, // a prefix of another key doesn't match
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
	if _, cmd := m.Update(agentsMsg{"fld-1": "working"}); cmd != nil {
		t.Error("agentsMsg returned a command")
	}
	if m.agents["fld-1"] != "working" {
		t.Errorf("agents = %v", m.agents)
	}
	q := &queueModel{details: map[string][]string{}}
	if _, cmd := q.Update(queueStateMsg{State{}, nil, true}); cmd != nil {
		t.Error("queueStateMsg with no items returned a command")
	}
}

func TestStaleDismissals(t *testing.T) {
	s := State{
		Dispatched: map[string]string{"FLD-1": "/w/1"},
		Dismissed:  map[string]string{"FLD-1": "a", "FLD-9": "b", "repo#2": "c", "repo#3": "d"},
	}
	prs := []Pr{{Key: "repo#2", Mine: true, Category: "Blocked"}, {Key: "repo#3", Mine: true, Category: "Done"}}
	got := staleDismissals(s, prs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"FLD-9", "repo#3"}) {
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
