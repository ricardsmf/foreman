package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
