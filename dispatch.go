package main

import (
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

	"golang.org/x/text/unicode/norm"
)

var (
	home, _      = os.UserHomeDir()
	repoRoots    = []string{filepath.Join(home, "work", "code"), filepath.Join(home, "projects")}
	stateFile    = filepath.Join(home, ".local", "state", "foreman", "state.json")
	mcpConfig    = filepath.Join(home, ".config", "mcp", "mcp.json")
	branchPrefix = "rf/"
)

type Agent struct{ Label, Note string }

var agents = []Agent{
	{"pi", "pi, default tools"},
	{"Claude", "claude, default permission prompts"},
	{"Claude (yolo)", "claude --dangerously-skip-permissions"},
}

type State struct {
	RepoByProject map[string]string `json:"repoByProject"`
	Agent         string            `json:"agent,omitempty"`
	Dispatched    map[string]string `json:"dispatched"`
}

func loadState() State {
	var s State
	if b, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.RepoByProject == nil {
		s.RepoByProject = map[string]string{}
	}
	if s.Dispatched == nil {
		s.Dispatched = map[string]string{}
	}
	return s
}

func saveState(s State) error {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(stateFile, b, 0o644)
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
	s := nonAlnum.ReplaceAllString(norm.NFKD.String(strings.ToLower(text)), "-")
	parts := strings.Split(strings.Trim(s, "-"), "-")
	return strings.Join(parts[:min(6, len(parts))], "-")
}

// sh runs a command and returns its stdout; a failure carries its stderr.
func sh(cwd, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return string(out), errors.New(msg)
		}
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

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
}

// dispatch creates or reuses the ticket's worktree and starts the agent on it in a new Rex session named after the ticket.
func dispatch(ctx context.Context, jira *Jira, d Dispatch, progress func(string)) (string, error) {
	issue, repo := d.Issue, d.Repo
	keyLower := strings.ToLower(issue.Key)
	git := func(args ...string) (string, error) { return sh("", "git", append([]string{"-C", repo}, args...)...) }

	cwd := repo
	out, err := git("branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if d.Worktree {
		cwd = repo + "-" + keyLower
		if exists(cwd) {
			out, err := sh("", "git", "-C", cwd, "branch", "--show-current")
			if err != nil {
				return "", err
			}
			branch = strings.TrimSpace(out)
			progress("Reusing worktree " + tilde(cwd))
		} else {
			progress("Fetching origin…")
			_, _ = git("fetch", "--quiet", "origin")
			out, err := git("branch", "-r", "--format=%(refname:short)")
			if err != nil {
				return "", err
			}
			remotes := strings.Split(out, "\n")
			keyRe := regexp.MustCompile(`(^|[^a-z0-9])` + regexp.QuoteMeta(keyLower) + `($|[^0-9])`)
			existing, base := "", ""
			for _, b := range remotes {
				if keyRe.MatchString(strings.ToLower(b)) {
					existing = b
					break
				}
			}
			for _, b := range []string{"origin/develop", "origin/main", "origin/master"} {
				if slices.Contains(remotes, b) {
					base = b
					break
				}
			}
			if existing != "" {
				branch = strings.TrimPrefix(existing, "origin/")
				if _, err := git("worktree", "add", "-B", branch, cwd, existing); err != nil {
					return "", err
				}
				_, _ = sh("", "git", "-C", cwd, "branch", "--set-upstream-to", existing)
				progress(fmt.Sprintf("Created worktree %s on %s", tilde(cwd), branch))
			} else {
				branch = branchPrefix + keyLower + "-" + slug(issue.Summary)
				args := []string{"worktree", "add", "--no-track", "-b", branch, cwd}
				if base != "" {
					args = append(args, base)
				}
				if _, err := git(args...); err != nil {
					return "", err
				}
				progress(fmt.Sprintf("Created worktree %s on %s from %s", tilde(cwd), branch, cmp(base, "HEAD")))
			}
		}
	}

	progress("Reading the ticket…")
	ticket, err := jira.detail(ctx, issue.Key)
	if err != nil {
		return "", err
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
		"4. **Commit** on this branch. Do not push, open a PR, or change the Jira ticket.",
		"5. **Finish** with a short summary: what changed, how you verified it, and anything left open.",
	)

	words := strings.Split(slug(issue.Summary), "-")
	label := keyLower + "-" + strings.Join(words[:min(3, len(words))], "-")
	session, err := start(d.Agent, strings.Join(lines, "\n"), cwd, label, progress)
	if err != nil {
		return "", err
	}
	markDispatched(issue.Key, cwd)
	return fmt.Sprintf("✓ %s is working on %s in session %s · %s (%s)", d.Agent, issue.Key, session, tilde(cwd), branch), nil
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
	if _, err := sh("", "git", "-C", repo, "fetch", "--quiet", "origin", v.BaseRefName, fmt.Sprintf("pull/%d/head", pr.Number)); err != nil {
		return "", err
	}
	head := v.HeadRefOid
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
		progress(fmt.Sprintf("Updated worktree %s to %s", tilde(cwd), head[:8]))
	} else {
		if _, err := sh("", "git", "-C", repo, "worktree", "add", "--detach", cwd, head); err != nil {
			return "", err
		}
		progress(fmt.Sprintf("Created worktree %s at %s", tilde(cwd), head[:8]))
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
		"4. **Report** in your final message: a verdict, then findings ordered by severity, each with `file:line`, what is wrong and a suggested fix. Separate blocking issues from nits.",
		"",
		"Do not commit, push, comment on, approve or request changes on the PR, or touch the Jira ticket. I'll post the review myself.",
	)

	session, err := start(d.Agent, strings.Join(lines, "\n"), cwd, "review-"+slug(pr.Key), progress)
	if err != nil {
		return "", err
	}
	markDispatched(pr.Key, cwd)
	return fmt.Sprintf("✓ %s is reviewing %s in session %s · %s", d.Agent, pr.Key, session, tilde(cwd)), nil
}

func start(agent, prompt, cwd, label string, progress func(string)) (string, error) {
	// Absolute paths: the fish `claude` function would otherwise add a second --mcp-config.
	var cmd []string
	if agent == "pi" {
		cmd = []string{which("pi"), prompt}
	} else {
		cmd = []string{which("claude"), "--mcp-config", mcpConfig}
		if agent == "Claude (yolo)" {
			cmd = append(cmd, "--dangerously-skip-permissions")
		}
		cmd = append(cmd, prompt)
	}
	progress(fmt.Sprintf("Starting %s…", agent))
	// `rex new` starts the session in the background; the app stays on the board.
	session, err := freeSessionLabel(label)
	if err != nil {
		return "", err
	}
	args := append([]string{"new", session, "--cwd", cwd, "--window", agent, "--"}, cmd...)
	if _, err := sh("", "rex", args...); err != nil {
		return "", err
	}
	return session, nil
}

func markDispatched(key, cwd string) {
	s := loadState()
	s.Dispatched[key] = cwd
	_ = saveState(s)
}

func freeSessionLabel(base string) (string, error) {
	out, err := sh("", "rex", "ls", "--json")
	if err != nil {
		return "", err
	}
	var data struct {
		Sessions []struct {
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, s := range data.Sessions {
		taken[s.Label] = true
	}
	label := base
	for n := 2; taken[label]; n++ {
		label = fmt.Sprintf("%s-%d", base, n)
	}
	return label, nil
}
