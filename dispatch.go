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
)

type Agent struct{ Label, Note string }

var agents = []Agent{
	{"pi", "pi, default tools"},
	{"Claude", "claude, default permission prompts"},
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
			return string(out), errors.New(msg)
		}
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
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

// prepareWorktree creates `<repo>-<key>` on the ticket's branch, or reuses it, and returns its branch.
func prepareWorktree(repo, cwd string, issue Issue, progress func(string)) (string, error) {
	git := func(args ...string) (string, error) { return sh("", "git", append([]string{"-C", repo}, args...)...) }
	if exists(cwd) {
		out, err := sh("", "git", "-C", cwd, "branch", "--show-current")
		if err != nil {
			return "", err
		}
		progress("Reusing worktree " + tilde(cwd))
		return strings.TrimSpace(out), nil
	}

	progress("Fetching origin…")
	if _, err := shFor(fetchTimeout, "", "git", "-C", repo, "fetch", "--quiet", "--prune", "origin"); err != nil {
		// Carry on from the refs we have, but say so: the branch may start from a stale base.
		progress("Fetch failed, using the last fetched refs: " + firstLine(err.Error()))
	}
	out, err := git("for-each-ref", "--sort=-committerdate", "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return "", err
	}
	remotes := strings.Fields(out)
	originHead, _ := git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")

	if existing := ticketBranch(remotes, issue.Key); existing != "" {
		branch := strings.TrimPrefix(existing, "origin/")
		// A local branch of that name may hold unpushed commits: check it out as is rather than reset it.
		if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			if _, err := git("worktree", "add", cwd, branch); err != nil {
				return "", err
			}
			progress(fmt.Sprintf("Created worktree %s on the local %s", tilde(cwd), branch))
			return branch, nil
		}
		if _, err := git("worktree", "add", "--track", "-b", branch, cwd, existing); err != nil {
			return "", err
		}
		progress(fmt.Sprintf("Created worktree %s on %s", tilde(cwd), branch))
		return branch, nil
	}

	base := baseBranch(remotes, strings.TrimSpace(originHead))
	branch := branchPrefix + strings.ToLower(issue.Key) + "-" + slug(issue.Summary)
	args := []string{"worktree", "add", "--no-track", "-b", branch, cwd}
	if base != "" {
		args = append(args, base)
	}
	if _, err := git(args...); err != nil {
		return "", err
	}
	progress(fmt.Sprintf("Created worktree %s on %s from %s", tilde(cwd), branch, cmp.Or(base, "HEAD")))
	return branch, nil
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// dispatch creates or reuses the ticket's worktree and starts the agent on it in a new Rex session named after the ticket.
func dispatch(ctx context.Context, jira *Jira, d Dispatch, progress func(string)) (string, error) {
	issue, repo := d.Issue, d.Repo
	keyLower := strings.ToLower(issue.Key)

	cwd := repo
	var branch string
	if d.Worktree {
		cwd = repo + "-" + keyLower
		b, err := prepareWorktree(repo, cwd, issue, progress)
		if err != nil {
			return "", err
		}
		branch = b
	} else {
		out, err := sh("", "git", "-C", repo, "branch", "--show-current")
		if err != nil {
			return "", err
		}
		branch = strings.TrimSpace(out)
		// The agent is told to commit on the branch it is on; that must not be the shared one.
		if branch == "" || slices.Contains(defaultBranches, branch) {
			return "", fmt.Errorf("%s is on %s; check out a ticket branch there or dispatch with a worktree",
				tilde(repo), cmp.Or(branch, "a detached HEAD"))
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
	lines = append(lines,
		"",
		"## Workflow",
		"",
		"1. **Understand.** Read the relevant code and the repo's AGENTS.md / CLAUDE.md before changing anything. If the ticket is ambiguous, ask me instead of guessing.",
		"2. **Implement** to the ticket's acceptance criteria. Keep scope to the ticket; respect its constraints and out-of-scope notes.",
		"3. **Verify.** Build, and run the tests and linters that cover what you touched.",
		"4. **Show it working.** Capture evidence that the change does what the ticket asks, as described below.",
		"5. **Commit** on this branch. Do not push, open a PR, or change the Jira ticket.",
		"6. **Report**: write the report below, then finish with the same summary in your final message.",
	)
	lines = append(lines, reportSection(issue.Key, true)...)

	words := strings.Split(slug(issue.Summary), "-")
	label := keyLower + "-" + strings.Join(words[:min(3, len(words))], "-")
	if err := resetEvidence(issue.Key); err != nil {
		return "", err
	}
	session, name, err := start(d.Agent, issue.Key, strings.Join(lines, "\n"), cwd, label, progress)
	if err != nil {
		return "", err
	}
	markDispatched(issue.Key, issue.Summary, cwd, session)
	return fmt.Sprintf("✓ %s is working on %s in session %s · %s (%s)", d.Agent, issue.Key, name, tilde(cwd), branch), nil
}

type ReviewDispatch struct {
	Pr      Pr
	Agent   string
	Context string
}

// dispatchReview checks the PR's head out detached in `<repo>-pr-<n>` (refreshed if it exists) and starts a read-only review.
func dispatchReview(ctx context.Context, jira *Jira, d ReviewDispatch, progress func(string)) (string, error) {
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
	session, name, err := start(d.Agent, pr.Key, strings.Join(lines, "\n"), cwd, "review-"+slug(pr.Key), progress)
	if err != nil {
		return "", err
	}
	markDispatched(pr.Key, v.Title, cwd, session)
	return fmt.Sprintf("✓ %s is reviewing %s in session %s · %s", d.Agent, pr.Key, name, tilde(cwd)), nil
}

// promptFile is where a dispatch leaves the agent's prompt, beside its report: a new dispatch and
// a cleanup remove it with the rest.
func promptFile(key string) string { return filepath.Join(evidenceDir(key), "prompt.md") }

// agentCommand runs the agent with the prompt read from file as its last argument. Rex caps a
// request at 64 KB, so a ticket with a long description or a PR with many files can't travel
// inside `rex new`; the shell reads it on the far side, where only ARG_MAX (1 MB) applies.
func agentCommand(agent, file string) []string {
	// Absolute paths: the fish `claude` function would otherwise add a second --mcp-config.
	var cmd []string
	if agent == "pi" {
		cmd = []string{which("pi")}
	} else {
		cmd = []string{which("claude"), "--mcp-config", mcpConfig}
		if agent == "Claude (yolo)" {
			cmd = append(cmd, "--dangerously-skip-permissions")
		}
		// The report and evidence go outside the working directory.
		cmd = append(cmd, "--add-dir", evidenceRoot)
	}
	return append([]string{"/bin/sh", "-c", `p=$(cat "$0") || exit; exec "$@" "$p"`, file}, cmd...)
}

// start returns the new session's ID, to track it by, and its label, to show.
func start(agent, key, prompt, cwd, label string, progress func(string)) (id, name string, err error) {
	file := promptFile(key)
	if err := os.WriteFile(file, []byte(prompt), 0o600); err != nil {
		return "", "", err
	}
	cmd := agentCommand(agent, file)
	progress(fmt.Sprintf("Starting %s…", agent))
	// `rex new` starts the session in the background; the app stays on the board.
	session, err := freeSessionLabel(label)
	if err != nil {
		return "", "", err
	}
	args := append([]string{"new", session, "--json", "--cwd", cwd, "--window", agent, "--"}, cmd...)
	out, err := sh("", "rex", args...)
	if err != nil {
		return "", "", err
	}
	// The ID, not the label: an agent may rename its session to describe the work.
	var made struct {
		ID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(out), &made) == nil && made.ID != "" {
		return made.ID, session, nil
	}
	return session, session, nil
}

func markDispatched(key, title, cwd, session string) {
	_, _ = updateState(func(s *State) {
		s.Dispatched[key] = cwd
		s.Sessions[key] = session
		s.Titles[key] = title
		delete(s.Dismissed, key)
		delete(s.Hidden, key)
	})
}

// evidenceDir is where the agent on a ticket or PR writes its report and evidence, e.g. …/evidence/fld-1671.
func evidenceDir(key string) string {
	return filepath.Join(evidenceRoot, strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(key), "-"), "-"))
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
