package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Only work checkouts: ~/projects holds third-party clones whose PRs are noise here.
var prRoot = filepath.Join(home, "work", "code")

// Pr is a PR in the board's row shape: key `repo#n`, status, author as assignee.
type Pr struct {
	Key       string
	Summary   string
	Status    string
	Category  string
	Assignee  string
	Repo      string // owner/name
	Number    int
	URL       string
	Branch    string // the head branch
	Checkout  string
	Requested bool
	Mine      bool
}

type PrView struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	HeadRefName       string `json:"headRefName"`
	BaseRefName       string `json:"baseRefName"`
	HeadRefOid        string `json:"headRefOid"`
	IsCrossRepository bool   `json:"isCrossRepository"` // from a fork
	Additions         int    `json:"additions"`
	Deletions         int    `json:"deletions"`
	ChangedFiles      int    `json:"changedFiles"`
	IsDraft           bool   `json:"isDraft"`
	ReviewDecision    string `json:"reviewDecision"`
	Mergeable         string `json:"mergeable"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Context    string `json:"context"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
	Files []struct {
		Path      string `json:"path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
}

var githubRemote = regexp.MustCompile(`(?i)github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)

// checkouts maps owner/name to the local checkout, for checkouts under prRoot whose origin is on GitHub.
func checkouts() map[string]string {
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, dir := range listRepos() {
		if !strings.HasPrefix(dir, prRoot+"/") {
			continue
		}
		wg.Go(func() {
			url, _ := sh("", "git", "-C", dir, "remote", "get-url", "origin")
			if m := githubRemote.FindStringSubmatch(strings.TrimSpace(url)); m != nil {
				mu.Lock()
				out[strings.ToLower(m[1])] = dir
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

var (
	loginMu sync.Mutex
	login   string
)

// ghLogin is the signed-in GitHub user. Only success is cached: a board started offline or before
// `gh auth login` must still load PRs once that is fixed.
func ghLogin() (string, error) {
	loginMu.Lock()
	defer loginMu.Unlock()
	if login != "" {
		return login, nil
	}
	s, err := sh("", "gh", "api", "user", "-q", ".login")
	if err != nil {
		return "", err
	}
	login = strings.TrimSpace(s)
	return login, nil
}

// One GraphQL request carries every search, so review requests and my PRs ride on the first page of
// all open PRs. Only mine ask for review, CI and merge state: on every PR those fields make the
// search several times slower. Search pages hold 100, so each further 100 PRs is another request.
const prsQuery = `query($all: String!, $requested: String!, $mine: String!, $first: Boolean!, $after: String) {
  all: search(query: $all, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest { number title url headRefName isDraft updatedAt repository { name nameWithOwner } author { __typename login } } }
  }
  requested: search(query: $requested, type: ISSUE, first: 100) @include(if: $first) {
    nodes { ... on PullRequest { number repository { nameWithOwner } } }
  }
  mine: search(query: $mine, type: ISSUE, first: 100) @include(if: $first) {
    nodes { ... on PullRequest {
      number reviewDecision mergeable repository { nameWithOwner }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
    } }
  }
}`

type prNode struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Branch     string `json:"headRefName"`
	IsDraft    bool   `json:"isDraft"`
	UpdatedAt  string `json:"updatedAt"`
	Repository struct {
		Name          string `json:"name"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
	ReviewDecision string `json:"reviewDecision"`
	Mergeable      string `json:"mergeable"`
	Commits        struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (p prNode) id() string {
	return strings.ToLower(fmt.Sprintf("%s#%d", p.Repository.NameWithOwner, p.Number))
}

// myStatus is my PR's status cell and colour category from its review, CI and merge state.
func myStatus(isDraft bool, own prNode) (string, string) {
	ci := ""
	if n := own.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		ci = n[0].Commit.StatusCheckRollup.State
	}
	mark := ""
	switch {
	case ci == "SUCCESS":
		mark = " ✓"
	case ci == "FAILURE" || ci == "ERROR":
		mark = " ✗"
	case ci != "":
		mark = " ●"
	}
	review := "in review"
	switch {
	case isDraft:
		review = "draft"
	case own.Mergeable == "CONFLICTING":
		review = "conflict"
	case own.ReviewDecision == "APPROVED":
		review = "approved"
	case own.ReviewDecision == "CHANGES_REQUESTED":
		review = "changes"
	}
	failing := mark == " ✗" || review == "changes" || review == "conflict"
	category := "To Do"
	switch {
	case failing:
		category = "Blocked"
	case review == "approved" && mark != " ●":
		category = "Done"
	case mark == " ●":
		category = "In Progress"
	}
	return review + mark, category
}

// maxPrs bounds the list: a search page holds 100, and each further page is another request.
const maxPrs = 500

// openPrs lists open PRs in repos I have checked out: review requests, then mine with their status,
// then other humans'. It loads up to maxPrs; truncated says GitHub had more.
func openPrs() (prs []Pr, truncated bool, err error) {
	if _, err := ghLogin(); err != nil {
		return nil, false, err
	}
	repos := checkouts()
	if len(repos) == 0 {
		return nil, false, nil
	}
	names := make([]string, 0, len(repos))
	for r := range repos {
		names = append(names, "repo:"+r)
	}
	scope := "is:pr is:open archived:false " + strings.Join(names, " ")

	var nodes, requested, mine []prNode
	after := ""
	for page := 0; ; page++ {
		args := []string{"api", "graphql", "-f", "query=" + prsQuery,
			"-f", "all=" + scope, "-f", "requested=" + scope + " review-requested:@me", "-f", "mine=" + scope + " author:@me"}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		args = append(args, "-F", "first="+strconv.FormatBool(page == 0))
		out, err := sh("", "gh", args...)
		if err != nil {
			return nil, false, err
		}
		var res struct {
			Data struct {
				All struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []prNode `json:"nodes"`
				} `json:"all"`
				Requested struct{ Nodes []prNode } `json:"requested"`
				Mine      struct{ Nodes []prNode } `json:"mine"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			return nil, false, err
		}
		nodes = append(nodes, res.Data.All.Nodes...)
		if page == 0 {
			requested, mine = res.Data.Requested.Nodes, res.Data.Mine.Nodes
		}
		if !res.Data.All.PageInfo.HasNextPage {
			break
		}
		if len(nodes) >= maxPrs {
			truncated = true
			break
		}
		after = res.Data.All.PageInfo.EndCursor
	}

	wanted := map[string]bool{}
	for _, p := range requested {
		wanted[p.id()] = true
	}
	status := map[string]prNode{}
	for _, p := range mine {
		status[p.id()] = p
	}
	type row struct {
		pr      Pr
		updated string
	}
	var rows []row
	for _, p := range nodes {
		if p.Number == 0 || (p.Author != nil && p.Author.Typename == "Bot") {
			continue
		}
		own, isMine := status[p.id()]
		isRequested := wanted[p.id()]
		var st, cat string
		switch {
		case isMine:
			st, cat = myStatus(p.IsDraft, own)
		case isRequested:
			st, cat = "requested", "In Progress"
		case p.IsDraft:
			st, cat = "draft", "To Do"
		default:
			st, cat = "open", "To Do"
		}
		author := ""
		if p.Author != nil {
			author = p.Author.Login
		}
		repo := p.Repository.NameWithOwner
		rows = append(rows, row{Pr{
			Key:       fmt.Sprintf("%s#%d", p.Repository.Name, p.Number),
			Summary:   p.Title,
			Status:    st,
			Category:  cat,
			Assignee:  author,
			Repo:      repo,
			Number:    p.Number,
			URL:       p.URL,
			Branch:    p.Branch,
			Checkout:  repos[strings.ToLower(repo)],
			Requested: isRequested,
			Mine:      isMine,
		}, p.UpdatedAt})
	}
	prRank := func(p Pr) int {
		if p.Requested {
			return 2
		}
		if p.Mine {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if d := prRank(b.pr) - prRank(a.pr); d != 0 {
			return d
		}
		return strings.Compare(b.updated, a.updated)
	})
	prs = make([]Pr, len(rows))
	for n, r := range rows {
		prs[n] = r.pr
	}
	return prs, truncated, nil
}

func viewPr(pr Pr) (PrView, error) {
	const fields = "title,body,url,author,headRefName,baseRefName,headRefOid,isCrossRepository,additions,deletions,changedFiles,isDraft,reviewDecision,mergeable,statusCheckRollup,files"
	var v PrView
	out, err := sh("", "gh", "pr", "view", strconv.Itoa(pr.Number), "-R", pr.Repo, "--json", fields)
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal([]byte(out), &v)
}

func prMarkdown(pr Pr, v PrView) string {
	var meta []string
	meta = append(meta,
		"Author: "+v.Author.Login,
		fmt.Sprintf("`%s` → `%s`", v.HeadRefName, v.BaseRefName),
		fmt.Sprintf("+%d −%d in %d files", v.Additions, v.Deletions, v.ChangedFiles))
	if v.IsDraft {
		meta = append(meta, "Draft")
	}
	if v.ReviewDecision != "" {
		meta = append(meta, "Review: "+strings.ReplaceAll(strings.ToLower(v.ReviewDecision), "_", " "))
	}
	if v.Mergeable == "CONFLICTING" {
		meta = append(meta, "Merge conflicts")
	}
	body := strings.TrimSpace(strings.ReplaceAll(v.Body, "\r", ""))
	if body == "" {
		body = "_No description._"
	}
	lines := []string{fmt.Sprintf("# %s: %s", pr.Key, v.Title), "", v.URL, strings.Join(meta, " · "), "", body, ""}
	lines = append(lines, checks(v)...)
	lines = append(lines, "## Files", "")
	for _, f := range v.Files {
		lines = append(lines, fmt.Sprintf("- `%s` +%d −%d", f.Path, f.Additions, f.Deletions))
	}
	return strings.Join(lines, "\n")
}

// checks is a Checks section listing failing and running checks, or one line when all pass.
func checks(v PrView) []string {
	if len(v.StatusCheckRollup) == 0 {
		return nil
	}
	badStates := []string{"FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED"}
	runningStates := []string{"", "PENDING", "QUEUED", "IN_PROGRESS", "EXPECTED", "WAITING"}
	var bad, running []string
	for _, c := range v.StatusCheckRollup {
		name := cmp.Or(c.Name, c.Context, "?")
		state := strings.ToUpper(cmp.Or(c.Conclusion, c.State, c.Status))
		if slices.Contains(badStates, state) {
			bad = append(bad, fmt.Sprintf("- ✗ **%s** %s", name, strings.ReplaceAll(strings.ToLower(state), "_", " ")))
		} else if slices.Contains(runningStates, state) {
			running = append(running, fmt.Sprintf("- ● %s running", name))
		}
	}
	lines := append(bad, running...)
	ok := len(v.StatusCheckRollup) - len(lines)
	out := append([]string{"## Checks", ""}, lines...)
	if len(lines) > 0 {
		out = append(out, "")
	}
	return append(out, fmt.Sprintf("%d/%d passed", ok, len(v.StatusCheckRollup)), "")
}

var jiraKeyRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)

// jiraKey is the first Jira key in the PR's title or branch, e.g. FLD-1574.
func jiraKey(pr Pr, v *PrView) string {
	branch := pr.Branch
	if v != nil {
		branch = v.HeadRefName
	}
	return jiraKeyRe.FindString(strings.ToUpper(pr.Summary + " " + branch))
}

// A PR's conversation, reviews and inline review threads: what a fix works from. One page of each;
// the markdown says when there is more.
const discussionQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(first: 100) { totalCount nodes { author { login } state body submittedAt } }
      comments(first: 100) { totalCount nodes { author { login } body createdAt } }
      reviewThreads(first: 100) { totalCount nodes {
        isResolved isOutdated path line originalLine
        comments(first: 50) { totalCount nodes { author { login } body createdAt } }
      } }
    }
  }
}`

type ghComment struct {
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	State       string `json:"state"` // a review's
	Body        string `json:"body"`
	CreatedAt   string `json:"createdAt"`
	SubmittedAt string `json:"submittedAt"`
}

func (c ghComment) login() string {
	if c.Author == nil {
		return "ghost"
	}
	return c.Author.Login
}

type ghThread struct {
	IsResolved   bool   `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	Comments     struct {
		TotalCount int         `json:"totalCount"`
		Nodes      []ghComment `json:"nodes"`
	} `json:"comments"`
}

type PrDiscussion struct {
	Reviews struct {
		TotalCount int         `json:"totalCount"`
		Nodes      []ghComment `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		TotalCount int         `json:"totalCount"`
		Nodes      []ghComment `json:"nodes"`
	} `json:"comments"`
	ReviewThreads struct {
		TotalCount int        `json:"totalCount"`
		Nodes      []ghThread `json:"nodes"`
	} `json:"reviewThreads"`
}

func prDiscussion(pr Pr) (PrDiscussion, error) {
	owner, name, _ := strings.Cut(pr.Repo, "/")
	out, err := sh("", "gh", "api", "graphql", "-f", "query="+discussionQuery,
		"-f", "owner="+owner, "-f", "name="+name, "-F", "number="+strconv.Itoa(pr.Number))
	if err != nil {
		return PrDiscussion{}, err
	}
	var res struct {
		Data struct {
			Repository struct {
				PullRequest *PrDiscussion `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return PrDiscussion{}, err
	}
	if res.Data.Repository.PullRequest == nil {
		return PrDiscussion{}, fmt.Errorf("GitHub has no PR %s#%d", pr.Repo, pr.Number)
	}
	return *res.Data.Repository.PullRequest, nil
}

// discussionMarkdown lists the reviews with something to say, the unresolved review threads and the
// conversation, each comment quoted under its author so its own headings can't break the prompt's.
func discussionMarkdown(d PrDiscussion) string {
	quote := func(c ghComment, when string) []string {
		lines := []string{fmt.Sprintf("**%s** (%s):", c.login(), cmp.Or(when, "?")), ""}
		for _, l := range strings.Split(strings.TrimSpace(strings.ReplaceAll(c.Body, "\r", "")), "\n") {
			lines = append(lines, strings.TrimRight("> "+l, " "))
		}
		return append(lines, "")
	}
	more := func(shown, total int, what string) []string {
		if total > shown {
			return []string{fmt.Sprintf("_%d more %s not shown; read them with `gh`._", total-shown, what), ""}
		}
		return nil
	}

	out := []string{"## Reviews", ""}
	n := 0
	for _, r := range d.Reviews.Nodes {
		// An empty COMMENTED review only carries inline comments, which the threads show.
		if r.State == "PENDING" || (strings.TrimSpace(r.Body) == "" && r.State != "CHANGES_REQUESTED") {
			continue
		}
		n++
		state := strings.ReplaceAll(strings.ToLower(r.State), "_", " ")
		if strings.TrimSpace(r.Body) == "" {
			out = append(out, fmt.Sprintf("**%s** (%s): %s, no comment.", r.login(), cmp.Or(r.SubmittedAt, "?"), state), "")
			continue
		}
		lines := quote(r, r.SubmittedAt)
		lines[0] = strings.TrimSuffix(lines[0], ":") + ", " + state + ":"
		out = append(out, lines...)
	}
	if n == 0 {
		out = append(out, "_None with a comment._", "")
	}
	out = append(out, more(len(d.Reviews.Nodes), d.Reviews.TotalCount, "reviews")...)

	resolved := 0
	var threads []string
	for _, t := range d.ReviewThreads.Nodes {
		if t.IsResolved {
			resolved++
			continue
		}
		where := "`" + t.Path + "`"
		if line := cmp.Or(t.Line, t.OriginalLine); line != 0 {
			where = fmt.Sprintf("`%s:%d`", t.Path, line)
		}
		if t.IsOutdated {
			where += " (outdated: the code moved since)"
		}
		threads = append(threads, "### "+where, "")
		for _, c := range t.Comments.Nodes {
			threads = append(threads, quote(c, c.CreatedAt)...)
		}
		threads = append(threads, more(len(t.Comments.Nodes), t.Comments.TotalCount, "replies")...)
	}
	out = append(out, "## Unresolved review threads", "")
	if len(threads) == 0 {
		out = append(out, "_None._", "")
	}
	out = append(out, threads...)
	if resolved > 0 {
		out = append(out, fmt.Sprintf("_%d resolved threads left out._", resolved), "")
	}
	out = append(out, more(len(d.ReviewThreads.Nodes), d.ReviewThreads.TotalCount, "threads")...)

	out = append(out, "## Conversation", "")
	if len(d.Comments.Nodes) == 0 {
		out = append(out, "_No comments._", "")
	}
	for _, c := range d.Comments.Nodes {
		out = append(out, quote(c, c.CreatedAt)...)
	}
	out = append(out, more(len(d.Comments.Nodes), d.Comments.TotalCount, "comments")...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
