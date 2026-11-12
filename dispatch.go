package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var (
	home, _   = os.UserHomeDir()
	repoRoots = []string{filepath.Join(home, "work", "code"), filepath.Join(home, "projects")}
	stateFile = filepath.Join(home, ".local", "state", "foreman", "state.json")
	// evidenceRoot holds each agent's report and screenshots, outside every repo so a commit can't pick them up.
	evidenceRoot = filepath.Join(home, ".local", "state", "foreman", "evidence")
	mcpConfig    = filepath.Join(home, ".config", "mcp", "mcp.json")
	// installedPlugins is Claude Code's record of its plugins; pi runs pstack from the same copy.
	installedPlugins = filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
)

type Agent struct{ Label, Note string }

var agents = []Agent{
	{agentPi, "pi, default tools"},
	{agentClaude, "claude, default permission prompts"},
	{"Claude (yolo)", "claude --dangerously-skip-permissions"},
}

func listRepos() []string {
	var repos []string
	for _, root := range repoRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
		for _, name := range names {
			dir := filepath.Join(root, name)
			// A main checkout has a .git directory; worktrees have a .git file.
			if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil && st.IsDir() {
				repos = append(repos, dir)
			}
		}
	}
	return repos
}

func tilde(p string) string { return strings.Replace(p, home, "~", 1) }

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slug(text string) string {
	// NFKD splits é into e and a combining accent; drop the accent so it doesn't become a dash.
	plain := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, norm.NFKD.String(strings.ToLower(text)))
	s := nonAlnum.ReplaceAllString(plain, "-")
	parts := strings.Split(strings.Trim(s, "-"), "-")
	return strings.Join(parts[:min(6, len(parts))], "-")
}

// Most commands answer in seconds; a fetch of a big repo can take minutes.
const (
	shTimeout    = time.Minute
	fetchTimeout = 3 * time.Minute
)

// sh runs a command and returns its stdout; a failure carries its stderr.
func sh(cwd, name string, args ...string) (string, error) {
	return shFor(shTimeout, cwd, name, args...)
}

func shFor(timeout time.Duration, cwd, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	// Without a delay, a killed git can leave its children holding the pipes open.
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("%s %s: timed out after %s", name, strings.Join(args, " "), timeout)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return string(out), errors.New(errorLines(msg))
		}
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// errorLines puts a command's fatal and error lines first: git prints progress before them
// ("Preparing worktree…"), and the status line shows only the first line.
func errorLines(stderr string) string {
	var errs, rest []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "fatal: ") || strings.HasPrefix(l, "error: ") {
			errs = append(errs, l)
		} else {
			rest = append(rest, l)
		}
	}
	return strings.Join(append(errs, rest...), "\n")
}

func short(sha string) string { return sha[:min(8, len(sha))] }

func which(bin string) string {
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	return bin
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

type Dispatch struct {
	Issue    Issue
	Agent    string
	Worktree bool
	Poteto   bool // the agent follows pstack's poteto-mode skill on top of the workflow
	Repo     string
	Context  string
	// Ticket is the ticket's markdown when the board already has it; empty means fetch it.
	Ticket string
}

var defaultBranches = []string{"develop", "main", "master"}

// baseBranch is the branch new work starts from: develop, else origin's HEAD, else main or master.
func baseBranch(remotes []string, originHead string) string {
	if slices.Contains(remotes, "origin/develop") {
		return "origin/develop"
	}
	if originHead != "" && slices.Contains(remotes, originHead) {
		return originHead
	}
	for _, b := range []string{"origin/main", "origin/master"} {
		if slices.Contains(remotes, b) {
			return b
		}
	}
	return ""
}

// ticketBranch is the origin branch named after the ticket, e.g. origin/li/FLD-1675-x for FLD-1675.
// remotes is sorted newest commit first, so an old merged branch loses to the live one.
func ticketBranch(remotes []string, key string) string {
	keyRe := regexp.MustCompile(`(^|[^a-z0-9])` + regexp.QuoteMeta(strings.ToLower(key)) + `($|[^0-9])`)
	for _, b := range remotes {
		if strings.HasPrefix(b, "origin/") && b != "origin/HEAD" && keyRe.MatchString(strings.ToLower(b)) {
			return b
		}
	}
	return ""
}

// prepareWorktree creates `<repo>-<key>` on the ticket's branch, or reuses it, and returns its
// directory and branch. A ticket branch already checked out elsewhere, in a worktree made by hand,
// is worked on there: git won't check a branch out twice.
func prepareWorktree(repo, cwd string, issue Issue, progress func(string)) (dir, branch string, err error) {
	git := func(args ...string) (string, error) { return sh("", "git", append([]string{"-C", repo}, args...)...) }
	if exists(cwd) {
		out, err := sh("", "git", "-C", cwd, "branch", "--show-current")
		if err != nil {
			return "", "", err
		}
		progress("Reusing worktree " + tilde(cwd))
		return cwd, strings.TrimSpace(out), nil
	}

	remotes, base, err := fetchOrigin(repo, progress)
	if err != nil {
		return "", "", err
	}
	if existing := ticketBranch(remotes, issue.Key); existing != "" {
		branch := strings.TrimPrefix(existing, "origin/")
		list, err := git("worktree", "list", "--porcelain")
		if err != nil {
			return "", "", err
		}
		if at := checkedOutAt(list, branch); at != "" {
			progress(fmt.Sprintf("Reusing %s, where %s is checked out", tilde(at), branch))
			return at, branch, nil
		}
		// A local branch of that name may hold unpushed commits: check it out as is rather than reset it.
		if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			if _, err := git("worktree", "add", cwd, branch); err != nil {
				return "", "", err
			}
			progress(fmt.Sprintf("Created worktree %s on the local %s", tilde(cwd), branch))
			return cwd, branch, nil
		}
		if _, err := git("worktree", "add", "--track", "-b", branch, cwd, existing); err != nil {
			return "", "", err
		}
		progress(fmt.Sprintf("Created worktree %s on %s", tilde(cwd), branch))
		return cwd, branch, nil
	}

	branch = branchPrefix + strings.ToLower(issue.Key) + "-" + slug(issue.Summary)
	if err := newBranchWorktree(repo, cwd, branch, base, progress); err != nil {
		return "", "", err
	}
	return cwd, branch, nil
}

// fetchOrigin fetches the repo's origin and returns its branches, newest commit first, and the
// one new work starts from.
func fetchOrigin(repo string, progress func(string)) (remotes []string, base string, err error) {
	progress("Fetching origin…")
	if _, err := shFor(fetchTimeout, "", "git", "-C", repo, "fetch", "--quiet", "--prune", "origin"); err != nil {
		// Carry on from the refs we have, but say so: the branch may start from a stale base.
		progress("Fetch failed, using the last fetched refs: " + firstLine(err.Error()))
	}
	out, err := sh("", "git", "-C", repo, "for-each-ref", "--sort=-committerdate", "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return nil, "", err
	}
	remotes = strings.Fields(out)
	originHead, _ := sh("", "git", "-C", repo, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	return remotes, baseBranch(remotes, strings.TrimSpace(originHead)), nil
}

// newBranchWorktree creates the worktree cwd on a new branch from base, or from HEAD without one.
func newBranchWorktree(repo, cwd, branch, base string, progress func(string)) error {
	args := []string{"-C", repo, "worktree", "add", "--no-track", "-b", branch, cwd}
	if base != "" {
		args = append(args, base)
	}
	if _, err := sh("", "git", args...); err != nil {
		return err
	}
	progress(fmt.Sprintf("Created worktree %s on %s from %s", tilde(cwd), branch, cmp.Or(base, "HEAD")))
	return nil
}

// prepareTaskWorktree creates cwd on a new branch named after the task's title, from where new
// work starts. A name already taken, here or on origin, gets a number: -2, -3…
func prepareTaskWorktree(repo, cwd, title string, n int, progress func(string)) (branch string, err error) {
	if exists(cwd) {
		return "", fmt.Errorf("%s already exists", tilde(cwd))
	}
	remotes, base, err := fetchOrigin(repo, progress)
	if err != nil {
		return "", err
	}
	name := branchPrefix + cmp.Or(slug(title), fmt.Sprintf("task-%d", n))
	branch = name
	for i := 2; ; i++ {
		_, local := sh("", "git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
		if local != nil && !slices.Contains(remotes, "origin/"+branch) {
			break
		}
		branch = fmt.Sprintf("%s-%d", name, i)
	}
	return branch, newBranchWorktree(repo, cwd, branch, base, progress)
}

// prepareFixWorktree puts the PR's head branch, which is on origin, in a worktree with origin's as
// its upstream: the worktree that already has it checked out, else cwd on the local branch of that
// name, else cwd on a new branch tracking origin's. reused says the worktree was there before, with
// whatever it holds.
func prepareFixWorktree(repo, cwd, head, base string, progress func(string)) (dir string, reused bool, err error) {
	git := func(args ...string) (string, error) { return sh("", "git", append([]string{"-C", repo}, args...)...) }
	// A branch made by hand may have no upstream; the agent is told it tracks origin's.
	track := func() error {
		if _, err := git("rev-parse", "--verify", "--quiet", head+"@{upstream}"); err == nil {
			return nil
		}
		_, err := git("branch", "--set-upstream-to=origin/"+head, head)
		return err
	}
	progress("Fetching " + head + "…")
	refspecs := []string{fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", head, head)}
	if base != "" {
		// The diff's left side, origin/<base>, should be current too.
		refspecs = append(refspecs, fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base))
	}
	if _, err := shFor(fetchTimeout, "", "git", append([]string{"-C", repo, "fetch", "--quiet", "origin"}, refspecs...)...); err != nil {
		return "", false, err
	}
	list, err := git("worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}
	if at := checkedOutAt(list, head); at != "" {
		progress(fmt.Sprintf("Reusing %s, where %s is checked out", tilde(at), head))
		return at, true, track()
	}
	if exists(cwd) {
		return "", false, fmt.Errorf("%s exists but isn't on %s; check %s out there or remove it", tilde(cwd), head, head)
	}
	// A local branch of that name may hold unpushed commits: check it out as is rather than reset it.
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+head); err == nil {
		if _, err := git("worktree", "add", cwd, head); err != nil {
			return "", false, err
		}
		progress(fmt.Sprintf("Created worktree %s on the local %s", tilde(cwd), head))
		return cwd, false, track()
	}
	if _, err := git("worktree", "add", "--track", "-b", head, cwd, "origin/"+head); err != nil {
		return "", false, err
	}
	progress(fmt.Sprintf("Created worktree %s on %s, tracking origin/%s", tilde(cwd), head, head))
	return cwd, false, nil
}

// checkedOutAt is the worktree that has branch checked out, from `git worktree list --porcelain`.
func checkedOutAt(list, branch string) string {
	dir := ""
	for _, line := range strings.Split(list, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			dir = p
		} else if line == "branch refs/heads/"+branch {
			return dir
		}
	}
	return ""
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// dispatch creates or reuses the ticket's worktree and starts the agent on it in a new Rex session named after the ticket.
func dispatch(ctx context.Context, jira *Jira, d Dispatch, progress func(string)) (string, error) {
	issue, repo := d.Issue, d.Repo
	keyLower := strings.ToLower(issue.Key)
	skill := ""
	if d.Poteto {
		var err error
		if skill, err = potetoSkill(); err != nil {
			return "", err
		}
	}

	cwd := repo
	var branch string
	if d.Worktree {
		dir, b, err := prepareWorktree(repo, repo+"-"+keyLower, issue, progress)
		if err != nil {
			return "", err
		}
		cwd, branch = dir, b
	} else {
		var err error
		if branch, err = workBranch(repo); err != nil {
			return "", err
		}
	}

	ticket := d.Ticket
	if ticket == "" {
		progress("Reading the ticket…")
		var err error
		if ticket, err = jira.detail(ctx, issue.Key); err != nil {
			return "", err
		}
	}
	from := headCommit(cwd)
	label := sessionLabel(issue.Key, issue.Summary)
	if err := resetEvidence(issue.Key); err != nil {
		return "", err
	}
	session, name, err := start(d.Agent, issue.Key, ticketPrompt(d, cwd, branch, ticket, skill), cwd, label, progress)
	if err != nil {
		return "", err
	}
	markDispatched(issue.Key, issue.Summary, cwd, session, d.Agent, from)
	return fmt.Sprintf("✓ %s is working on %s in session %s · %s (%s)", d.Agent, issue.Key, name, tilde(cwd), branch), nil
}

// workBranch is the branch the checkout is on, for an agent that works in it without a worktree.
// The agent is told to commit on it, so it must not be the shared one.
func workBranch(repo string) (string, error) {
	out, err := sh("", "git", "-C", repo, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "" || slices.Contains(defaultBranches, branch) {
		return "", fmt.Errorf("%s is on %s; check out a work branch there or dispatch with a worktree",
			tilde(repo), cmp.Or(branch, "a detached HEAD"))
	}
	return branch, nil
}

// A task is work I describe myself, with no ticket or PR behind it. Its key, task:<repo>-<n>,
// reads as neither a Jira key nor a PR's (`#`).
const taskPrefix = "task:"

func isTask(key string) bool { return strings.HasPrefix(key, taskPrefix) }

// A fix's key is its PR's with this prefix, fix:reports-app#1260: a review of the same PR keeps
// its own state, session and report.
const fixPrefix = "fix:"

func isFix(key string) bool { return strings.HasPrefix(key, fixPrefix) }

func fixKey(prKey string) string { return fixPrefix + prKey }

type TaskDispatch struct {
	Repo     string
	Agent    string
	Worktree bool
	Poteto   bool
	// Title names the task in its session, branch and the queue; empty takes the task's first line.
	Title string
	Task  string
}

// taskTitle is the task's first line, short enough for the queue.
func taskTitle(task string) string {
	r := []rune(strings.TrimSpace(firstLine(strings.TrimSpace(task))))
	if len(r) > 72 {
		return string(r[:71]) + "…"
	}
	return string(r)
}

// newTaskKey reserves the repo's next task number. Numbers are never reused, so a new task's
// worktree and report can't land on an old one's.
func newTaskKey(repo string) (key string, n int, err error) {
	name := filepath.Base(repo)
	_, err = updateState(func(s *State) {
		s.TaskSeq[name]++
		n = s.TaskSeq[name]
	})
	return fmt.Sprintf("%s%s-%d", taskPrefix, name, n), n, err
}

// dispatchTask starts the agent on a task in a new worktree, `<repo>-task-<n>`, or in the checkout
// itself, in a new Rex session named after the task.
func dispatchTask(d TaskDispatch, progress func(string)) (string, error) {
	task := strings.TrimSpace(d.Task)
	if task == "" {
		return "", errors.New("describe the task first")
	}
	title := cmp.Or(strings.TrimSpace(d.Title), taskTitle(task))
	skill := ""
	if d.Poteto {
		var err error
		if skill, err = potetoSkill(); err != nil {
			return "", err
		}
	}
	cwd, branch := d.Repo, ""
	if !d.Worktree {
		var err error
		if branch, err = workBranch(d.Repo); err != nil {
			return "", err
		}
	}
	key, n, err := newTaskKey(d.Repo)
	if err != nil {
		return "", err
	}
	if d.Worktree {
		cwd = fmt.Sprintf("%s-task-%d", d.Repo, n)
		if branch, err = prepareTaskWorktree(d.Repo, cwd, title, n, progress); err != nil {
			return "", err
		}
	}
	if err := resetEvidence(key); err != nil {
		return "", err
	}
	from := headCommit(cwd)
	prompt := taskPrompt(key, title, task, d.Agent, cwd, branch, d.Worktree, skill)
	session, name, err := start(d.Agent, key, prompt, cwd, sessionLabel(key, title), progress)
	if err != nil {
		return "", err
	}
	markDispatched(key, title, cwd, session, d.Agent, from)
	return fmt.Sprintf("✓ %s is working on %s in session %s · %s (%s)", d.Agent, key, name, tilde(cwd), branch), nil
}

// taskPrompt is what the agent on a task starts with. skill is the poteto-mode skill's path, or
// empty to leave the skill out.
func taskPrompt(key, title, task, agent, cwd, branch string, worktree bool, skill string) string {
	note := ""
	if worktree {
		note = " (a git worktree created for this task)"
	}
	lines := []string{
		fmt.Sprintf("# %s: %q", key, title),
		"",
		"You are working on a task I'm giving you directly; there is no Jira ticket behind it.",
		"Working directory: " + cwd,
		"Branch: " + branch + note,
		"",
		"## Task",
		"",
		task,
	}
	lines = append(lines, skillsSection(agent, cwd)...)
	lines = append(lines, methodSection(skill, "task")...)
	lines = append(lines,
		"",
		"## Workflow",
		"",
		"1. **Understand.** Read the relevant code and the repo's AGENTS.md / CLAUDE.md before changing anything. If the task is ambiguous, ask instead of guessing, as below.",
		"2. **Do the task**, and only it. If it asks a question or an investigation rather than a change, answer it in the report and leave the code as it is.",
		"3. **Verify** a change: build, and run the tests and linters that cover what you touched.",
		"4. **Show it working.** Capture evidence that the change does what the task asks, as described below.",
		"5. **Commit** a change on this branch. Do not push, open a PR, or post to GitHub or Jira.",
		"6. **Report**: write the report below, then finish with the same summary in your final message.",
	)
	lines = append(lines, askSection()...)
	lines = append(lines, reportSection(key, true)...)
	return strings.Join(lines, "\n")
}

// methodSection points the agent at the poteto-mode skill, under the workflow that follows it.
func methodSection(skill, what string) []string {
	if skill == "" {
		return nil
	}
	return []string{
		"",
		"## Method",
		"",
		"Work in poteto-mode: read `" + skill + "` first and follow it, with the playbook that fits this " + what + ".",
		"Where it differs from the workflow below, the workflow wins: ask me when the " + what + " is ambiguous, and end with the commit and the report in place of the Opening a PR playbook.",
	}
}

// sessionLabel names a dispatch's Rex session: a ticket's key and the start of its summary
// (fld-1671-my-van-always), a review's PR key (review-reports-app-1185), a task's key and the start
// of its title (task-foreman-3-add-a-dispatch). findSession relies on the key leading it.
// A fix's key and the start of its title lead its label the same way
// (fix-reports-app-1260-address-review-comments).
func sessionLabel(key, summary string) string {
	if strings.Contains(key, "#") && !isFix(key) {
		return "review-" + slug(key)
	}
	label := strings.ToLower(key)
	if isTask(key) || isFix(key) {
		label = keySlug(key)
	}
	if s := slug(summary); s != "" {
		words := strings.Split(s, "-")
		label += "-" + strings.Join(words[:min(3, len(words))], "-")
	}
	return label
}

// ticketPrompt is what the agent starts with. skill is the poteto-mode skill's path, or empty to
// leave the skill out.
func ticketPrompt(d Dispatch, cwd, branch, ticket, skill string) string {
	issue := d.Issue
	note := ""
	if d.Worktree {
		note = " (a git worktree created for this ticket)"
	}
	lines := []string{
		fmt.Sprintf("# %s: %q", issue.Key, issue.Summary),
		"",
		fmt.Sprintf("You are implementing Jira ticket %s.", issue.Key),
		"Working directory: " + cwd,
		"Branch: " + branch + note,
		"",
		"## Ticket",
		"",
		ticket,
	}
	if d.Context != "" {
		lines = append(lines, "", "## Extra context from me", "", d.Context)
	}
	lines = append(lines, skillsSection(d.Agent, cwd)...)
	lines = append(lines, methodSection(skill, "ticket")...)
	lines = append(lines,
		"",
		"## Workflow",
		"",
		"1. **Understand.** Read the relevant code and the repo's AGENTS.md / CLAUDE.md before changing anything. If the ticket is ambiguous, ask instead of guessing, as below.",
		"2. **Implement** to the ticket's acceptance criteria. Keep scope to the ticket; respect its constraints and out-of-scope notes.",
		"3. **Verify.** Build, and run the tests and linters that cover what you touched.",
		"4. **Show it working.** Capture evidence that the change does what the ticket asks, as described below.",
		"5. **Commit** on this branch. Do not push, open a PR, or change the Jira ticket.",
		"6. **Report**: write the report below, then finish with the same summary in your final message.",
	)
	lines = append(lines, askSection()...)
	lines = append(lines, reportSection(issue.Key, true)...)
	return strings.Join(lines, "\n")
}

// potetoSkill is the installed pstack's poteto-mode skill. Its path holds the plugin's version,
// so each dispatch looks it up.
func potetoSkill() (string, error) {
	b, err := os.ReadFile(installedPlugins)
	if err != nil {
		return "", fmt.Errorf("poteto-mode: %w", err)
	}
	var v struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", fmt.Errorf("poteto-mode: %s: %w", tilde(installedPlugins), err)
	}
	for _, p := range v.Plugins["pstack@cursor-plugins"] {
		if skill := filepath.Join(p.InstallPath, "skills", "poteto-mode", "SKILL.md"); exists(skill) {
			return skill, nil
		}
	}
	return "", fmt.Errorf("poteto-mode: %s lists no pstack plugin with the skill", tilde(installedPlugins))
}

type PrDispatch struct {
	Pr      Pr
	Agent   string
	Context string
	// Fix has the agent fix the PR on its branch rather than review it.
	Fix bool
	// Title names a fix in its session and the queue; empty takes the PR's title.
	Title string
}

// dispatchPr starts a fix or a review of the PR.
func dispatchPr(ctx context.Context, jira *Jira, d PrDispatch, progress func(string)) (string, error) {
	if d.Fix {
		return dispatchFix(ctx, jira, d, progress)
	}
	return dispatchReview(ctx, jira, d, progress)
}

// dispatchReview checks the PR's head out detached in `<repo>-pr-<n>` (refreshed if it exists) and starts a read-only review.
func dispatchReview(ctx context.Context, jira *Jira, d PrDispatch, progress func(string)) (string, error) {
	pr := d.Pr
	repo := pr.Checkout
	cwd := fmt.Sprintf("%s-pr-%d", repo, pr.Number)
	progress(fmt.Sprintf("Reading %s…", pr.Key))
	v, err := viewPr(pr)
	if err != nil {
		return "", err
	}

	progress("Fetching the PR head…")
	// Naming the base updates origin/<base> too, so the diff's left side is current.
	if _, err := shFor(fetchTimeout, "", "git", "-C", repo, "fetch", "--quiet", "origin", v.BaseRefName, fmt.Sprintf("pull/%d/head", pr.Number)); err != nil {
		return "", err
	}
	head := v.HeadRefOid
	if head == "" {
		return "", errors.New("GitHub returned no head commit for the PR")
	}
	if exists(cwd) {
		dirty, err := sh("", "git", "-C", cwd, "status", "--porcelain")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(dirty) != "" {
			return "", fmt.Errorf("%s has uncommitted changes; clean it up or remove the worktree", tilde(cwd))
		}
		if _, err := sh("", "git", "-C", cwd, "checkout", "--quiet", "--detach", head); err != nil {
			return "", err
		}
		progress(fmt.Sprintf("Updated worktree %s to %s", tilde(cwd), short(head)))
	} else {
		if _, err := sh("", "git", "-C", repo, "worktree", "add", "--detach", cwd, head); err != nil {
			return "", err
		}
		progress(fmt.Sprintf("Created worktree %s at %s", tilde(cwd), short(head)))
	}

	key := jiraKey(pr, &v)
	ticket := ""
	if key != "" {
		progress(fmt.Sprintf("Reading %s…", key))
		ticket, _ = jira.detail(ctx, key)
	}
	base := "origin/" + v.BaseRefName
	lines := []string{
		fmt.Sprintf("# Review %s#%d: %q", pr.Repo, pr.Number, v.Title),
		"",
		fmt.Sprintf("You are reviewing a pull request by %s. Do not change it.", v.Author.Login),
		"Working directory: " + cwd,
		fmt.Sprintf("Checked out: the PR head %s (detached), a git worktree created for this review.", v.HeadRefOid),
		fmt.Sprintf("Diff to review: `git diff %s...HEAD` (`%s` → `%s`).", base, v.HeadRefName, v.BaseRefName),
		"",
		"## Pull request",
		"",
		prMarkdown(pr, v),
	}
	if ticket != "" {
		lines = append(lines, "", "## Jira ticket", "", ticket)
	} else if key != "" {
		lines = append(lines, "", fmt.Sprintf("The PR mentions %s, but the ticket could not be read.", key))
	}
	if d.Context != "" {
		lines = append(lines, "", "## Extra context from me", "", d.Context)
	}
	lines = append(lines, skillsSection(d.Agent, cwd)...)
	acceptance, against := "", ""
	if ticket != "" {
		acceptance, against = ", the ticket's acceptance criteria", "; check it against the ticket"
	}
	lines = append(lines,
		"",
		"## Workflow",
		"",
		"1. **Understand.** Read the repo's AGENTS.md / CLAUDE.md, the PR description"+acceptance+", and the code around each change.",
		"2. **Review** the diff for correctness, missed cases, regressions, security, tests, and fit with the codebase's conventions"+against+".",
		"3. **Verify** where it helps: run the tests and linters that cover the changed code. Don't edit tracked files.",
		"4. **Report**: a verdict, then findings ordered by severity, each with `file:line`, what is wrong and a suggested fix. Separate blocking issues from nits. Write it as described below and end your final message with it.",
		"",
		"Do not commit, push, comment on, approve or request changes on the PR, or touch the Jira ticket. I'll post the review myself.",
	)
	lines = append(lines, reportSection(pr.Key, false)...)

	if err := resetEvidence(pr.Key); err != nil {
		return "", err
	}
	session, name, err := start(d.Agent, pr.Key, strings.Join(lines, "\n"), cwd, sessionLabel(pr.Key, ""), progress)
	if err != nil {
		return "", err
	}
	// A review changes no code, so it has no work to diff from a start.
	markDispatched(pr.Key, v.Title, cwd, session, d.Agent, "")
	return fmt.Sprintf("✓ %s is reviewing %s in session %s · %s", d.Agent, pr.Key, name, tilde(cwd)), nil
}

// dispatchFix starts an agent that fixes the PR on its own branch: in the worktree that has the
// branch checked out, else in a new `<repo>-fix-<n>` tracking origin's.
func dispatchFix(ctx context.Context, jira *Jira, d PrDispatch, progress func(string)) (string, error) {
	pr := d.Pr
	key := fixKey(pr.Key)
	progress(fmt.Sprintf("Reading %s…", pr.Key))
	v, err := viewPr(pr)
	if err != nil {
		return "", err
	}
	if v.IsCrossRepository {
		return "", fmt.Errorf("%s is from a fork (%s:%s); a fix works only on a PR whose branch is on origin", pr.Key, v.Author.Login, v.HeadRefName)
	}
	if v.HeadRefName == "" {
		return "", errors.New("GitHub returned no head branch for the PR")
	}
	cwd, reused, err := prepareFixWorktree(pr.Checkout, fmt.Sprintf("%s-fix-%d", pr.Checkout, pr.Number), v.HeadRefName, v.BaseRefName, progress)
	if err != nil {
		return "", err
	}
	progress("Reading the review comments…")
	discussion, err := prDiscussion(pr)
	if err != nil {
		return "", fmt.Errorf("review comments: %w", err)
	}
	ticketKey, ticket := jiraKey(pr, &v), ""
	if ticketKey != "" {
		progress(fmt.Sprintf("Reading %s…", ticketKey))
		ticket, _ = jira.detail(ctx, ticketKey)
	}
	title := cmp.Or(strings.TrimSpace(d.Title), v.Title)
	if err := resetEvidence(key); err != nil {
		return "", err
	}
	from := headCommit(cwd)
	prompt := fixPrompt(d, v, discussionMarkdown(discussion), ticketKey, ticket, cwd, reused)
	session, name, err := start(d.Agent, key, prompt, cwd, sessionLabel(key, title), progress)
	if err != nil {
		return "", err
	}
	markDispatched(key, title, cwd, session, d.Agent, from)
	return fmt.Sprintf("✓ %s is fixing %s in session %s · %s (%s)", d.Agent, pr.Key, name, tilde(cwd), v.HeadRefName), nil
}

// fixPrompt is what the agent fixing a PR starts with. discussion is the PR's reviews and comments
// as markdown; ticket is the ticket named by ticketKey, empty when it couldn't be read.
func fixPrompt(d PrDispatch, v PrView, discussion, ticketKey, ticket, cwd string, reused bool) string {
	pr := d.Pr
	head, base := v.HeadRefName, v.BaseRefName
	where := "a git worktree created for this fix"
	if reused {
		where = "a worktree that already had it checked out, so it may hold work of its own: check `git status`, and `git log` against `origin/" + head + "`, before you start"
	}
	lines := []string{
		fmt.Sprintf("# Fix %s#%d: %q", pr.Repo, pr.Number, v.Title),
		"",
		fmt.Sprintf("You are fixing a pull request by %s on its own branch.", v.Author.Login),
		"Working directory: " + cwd,
		fmt.Sprintf("Branch: %s, tracking origin/%s (%s).", head, head, where),
		fmt.Sprintf("The PR's changes: `git diff origin/%s...HEAD` (`%s` → `%s`).", base, head, base),
	}
	if d.Context != "" {
		lines = append(lines, "", "## What to fix", "", d.Context)
	}
	lines = append(lines, "", "## Pull request", "", prMarkdown(pr, v), "", discussion)
	if ticket != "" {
		lines = append(lines, "", "## Jira ticket", "", ticket)
	} else if ticketKey != "" {
		lines = append(lines, "", fmt.Sprintf("The PR mentions %s, but the ticket could not be read.", ticketKey))
	}
	lines = append(lines, skillsSection(d.Agent, cwd)...)
	scope := "Address each unresolved review thread and each review comment that asks for a change."
	if d.Context != "" {
		scope = "Do what the What to fix section asks; the review comments are its context."
	}
	acceptance := ""
	if ticket != "" {
		acceptance = ", the ticket's acceptance criteria"
	}
	lines = append(lines,
		"",
		"## Workflow",
		"",
		"1. **Understand.** Read the repo's AGENTS.md / CLAUDE.md, the PR description"+acceptance+", the review comments and the code they point at. If a comment is ambiguous, ask instead of guessing, as below.",
		"2. **Fix.** "+scope+" Keep to the PR's scope. A comment you think is wrong or already handled: leave the code as it is and say why in the report, so I can answer the reviewer.",
		"3. **Verify.** Build, and run the tests and linters that cover what you touched.",
		"4. **Show it working.** Capture evidence that the fix does what the comments ask, as described below.",
		"5. **Commit** on this branch, on top of the PR's commits, leaving them as they are.",
		"6. **Report**: for each comment, what you changed or why you didn't, then how you verified it. Write it as described below and end your final message with it.",
		"",
		"Leave pushing and answering the reviewers to me: do not push, reply to or resolve comments, or change the PR or the Jira ticket.",
	)
	lines = append(lines, askSection()...)
	lines = append(lines, reportSection(fixKey(pr.Key), true)...)
	return strings.Join(lines, "\n")
}

// promptFile is where a dispatch leaves the agent's prompt, beside its report: a new dispatch and
// a cleanup remove it with the rest.
func promptFile(key string) string { return filepath.Join(evidenceDir(key), "prompt.md") }

// agentCommand runs the agent with the prompt read from file as its last argument. Rex caps a
// request at 64 KB, so a ticket with a long description or a PR with many files can't travel
// inside `rex new`; the shell reads it on the far side, where only ARG_MAX (1 MB) applies.
func agentCommand(agent, file string) []string {
	// Absolute paths: the fish `claude` function would otherwise add a second --mcp-config.
	cmd := agentArgs(agent)
	if agent != agentPi {
		// --add-dir takes a list, so without `--` it swallows the prompt as a second directory.
		cmd = append(cmd, "--")
	}
	return append([]string{"/bin/sh", "-c", `p=$(cat "$0") || exit; exec "$@" "$p"`, file}, cmd...)
}

// agentArgs starts the agent without a prompt.
func agentArgs(agent string) []string {
	if agent == agentPi {
		return []string{which("pi")}
	}
	cmd := []string{which("claude"), "--mcp-config", mcpConfig}
	if agent == "Claude (yolo)" {
		cmd = append(cmd, "--dangerously-skip-permissions")
	}
	// foreman ask without a permission prompt: a prompt would park the question on me.
	cmd = append(cmd, "--allowedTools", "Bash(foreman ask:*)")
	// The report and evidence go outside the working directory.
	return append(cmd, "--add-dir", evidenceRoot)
}

// askSection tells an implementing agent to put its questions through foreman ask, which waits for
// the orchestrator's answer or mine.
func askSection() []string {
	return []string{
		"",
		"## Asking",
		"",
		"When you need an answer from me to go on, run `foreman ask \"<question>\"`, giving the command a 10-minute timeout. My orchestrator answers what I've already settled and hands the rest to me; the command prints the answer and you carry on.",
		"If it reports no answer, put the question in your report and stop to ask me.",
	}
}

// resumeCommand continues the agent's latest conversation in its working directory. A worktree is
// the ticket's alone, so that conversation is the dispatched one.
func resumeCommand(agent string) []string {
	cmd := agentArgs(agent)
	// Before --add-dir, which takes a list.
	return slices.Insert(cmd, 1, "--continue")
}

// dispatchedAgent is the agent to resume key with: the one recorded at dispatch, else, for
// dispatches from before agents were recorded, the one with the newest conversation in dir.
func dispatchedAgent(s State, key, dir string) (string, error) {
	if a := s.Agents[key]; a != "" {
		return a, nil
	}
	var newest time.Time
	agent := ""
	for _, c := range []struct{ agent, dir string }{
		{agentClaude, claudeProjectDir(dir)},
		{agentPi, piSessionDir(dir)},
	} {
		if st, err := os.Stat(c.dir); err == nil && st.ModTime().After(newest) {
			newest, agent = st.ModTime(), c.agent
		}
	}
	if agent == "" {
		return "", fmt.Errorf("no pi or Claude conversation in %s to resume", tilde(dir))
	}
	return agent, nil
}

// claudeProjectDir is where Claude Code keeps the conversations started in dir.
func claudeProjectDir(dir string) string {
	return filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(dir, "-"))
}

// piSessionDir is where pi keeps the sessions started in dir.
func piSessionDir(dir string) string {
	name := "--" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(strings.TrimLeft(dir, "/\\")) + "--"
	return filepath.Join(home, ".pi", "agent", "sessions", name)
}

// start returns the new session's ID, to track it by, and its label, to show.
func start(agent, key, prompt, cwd, label string, progress func(string)) (id, name string, err error) {
	file := promptFile(key)
	if err := os.WriteFile(file, []byte(prompt), 0o600); err != nil {
		return "", "", err
	}
	progress(fmt.Sprintf("Starting %s…", agent))
	return newSession(label, cwd, agent, agentCommand(agent, file))
}

// headCommit is the commit dir is on, "" when git can't tell (a repo with no commits yet).
func headCommit(dir string) string {
	out, err := sh(dir, "git", "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// markDispatched records the dispatch. from is the commit the agent starts from, "" for none.
func markDispatched(key, title, cwd, session, agent, from string) {
	_, _ = updateState(func(s *State) {
		s.Dispatched[key] = cwd
		if from != "" {
			s.StartedFrom[key] = from
		} else {
			delete(s.StartedFrom, key)
		}
		s.Sessions[key] = session
		s.Agents[key] = agent
		s.Titles[key] = title
		delete(s.Questions, key)
		delete(s.Dismissed, key)
		delete(s.Hidden, key)
	})
}

// evidenceDir is where the agent on a ticket or PR writes its report and evidence, e.g. …/evidence/fld-1671.
func evidenceDir(key string) string { return filepath.Join(evidenceRoot, keySlug(key)) }

// keySlug is the key in lower case with dashes for everything else, whole: slug keeps six words,
// and a task in a repo with a long name has more.
func keySlug(key string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(key), "-"), "-")
}

// resetEvidence empties the key's evidence directory, so the queue never shows a previous run's report.
func resetEvidence(key string) error {
	dir := evidenceDir(key)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// reportSection tells the agent where its report and evidence go and that they are for me alone.
func reportSection(key string, implementing bool) []string {
	dir := evidenceDir(key)
	out := []string{
		"",
		"## Report and evidence",
		"",
		"My review queue reads your report from `" + dir + "/report.md`. Write it when you finish, and also before you stop to ask me something, so the question is in it. Start it with a `# ` line that states the outcome in one sentence.",
	}
	if implementing {
		out = append(out,
			"The report says what changed, how you verified it, and anything left open.",
			"",
			"Save evidence in the same directory, so I can check the change without running it:",
			"",
			"- **Web UI**: a screenshot per acceptance criterion, `agent-browser screenshot "+dir+"/01-<what>.png`. For a flow or an interaction, also a short video: `agent-browser record start "+dir+"/demo.webm <url>`, go through it, then `agent-browser record stop`.",
			"- **Terminal UI**: the rendered screen, `rex capture -b <block> --trim > "+dir+"/<what>.txt`.",
			"- **No UI** (API, jobs, libraries): no media; put the commands you ran and their output in the report.",
		)
	} else {
		out = append(out, "If you ran the change to check it, save screenshots or terminal captures in the same directory as well.")
	}
	return append(out,
		"",
		"That directory is outside the repo and is for me only. Never commit, attach, upload or link these files in a PR, the Jira ticket or a comment.",
	)
}
