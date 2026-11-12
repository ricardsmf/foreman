package main

// foreman dispatch: the board's dispatch without the board, for scripts and for an agent that
// hands work out.

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const dispatchUsage = `usage: foreman dispatch [options] [TEXT | -]

With neither --ticket nor --pr, TEXT is the task and --repo is required. With either, TEXT is
optional extra context. "-" reads TEXT from stdin. Options go before TEXT.

  --repo NAME     a repo under ~/work/code or ~/projects, by directory name or path
                  (a ticket defaults to its project's last repo)
  --ticket KEY    work on a Jira ticket
  --pr REPO#N     review a pull request, e.g. reports-app#1185
  --fix           with --pr: fix the PR on its own branch (its review comments, or TEXT) instead
                  of reviewing it, in the worktree that has the branch, else <repo>-fix-<n>
  --agent NAME    pi, claude or yolo (default: the repo's last, else Claude where it ships skills)
  --no-worktree   work in the checkout itself, which must be on a work branch
  --poteto        follow pstack's poteto-mode skill
  --title TITLE   the task's title (default: TEXT's first line), or a fix's (default: the PR's)
  --again         start even though the ticket's or PR's agent session is still open`

type dispatchArgs struct {
	repo, ticket, pr, agent, title, text string
	noWorktree, poteto, again, fix       bool
}

func parseDispatchArgs(argv []string, stdin io.Reader) (dispatchArgs, error) {
	var a dispatchArgs
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.repo, "repo", "", "")
	fs.StringVar(&a.ticket, "ticket", "", "")
	fs.StringVar(&a.pr, "pr", "", "")
	fs.StringVar(&a.agent, "agent", "", "")
	fs.StringVar(&a.title, "title", "", "")
	fs.BoolVar(&a.noWorktree, "no-worktree", false, "")
	fs.BoolVar(&a.poteto, "poteto", false, "")
	fs.BoolVar(&a.again, "again", false, "")
	fs.BoolVar(&a.fix, "fix", false, "")
	if err := fs.Parse(argv); err != nil {
		return a, err
	}
	a.text = strings.Join(fs.Args(), " ")
	if a.text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return a, err
		}
		a.text = string(b)
	}
	a.text = strings.TrimSpace(a.text)
	switch {
	case a.ticket != "" && a.pr != "":
		return a, errors.New("--ticket and --pr don't go together")
	case a.ticket == "" && a.pr == "" && a.text == "":
		return a, errors.New("a task needs TEXT saying what to do")
	case a.ticket == "" && a.pr == "" && a.repo == "":
		return a, errors.New("a task needs --repo")
	case a.fix && a.pr == "":
		return a, errors.New("--fix goes with --pr")
	case a.fix && (a.repo != "" || a.noWorktree || a.poteto):
		return a, errors.New("a fix takes only --agent, --title, --again and TEXT")
	case a.pr != "" && !a.fix && (a.repo != "" || a.noWorktree || a.poteto || a.title != ""):
		return a, errors.New("a review takes only --agent, --again and TEXT")
	case a.ticket != "" && a.title != "":
		return a, errors.New("--title is for a task; a ticket has its summary")
	}
	return a, nil
}

// resolveRepo finds a repo among repos by path or by directory name.
func resolveRepo(arg string, repos []string) (string, error) {
	path := arg
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		path = filepath.Join(home, rest)
	}
	if abs, err := filepath.Abs(path); err == nil && slices.Contains(repos, filepath.Clean(abs)) {
		return filepath.Clean(abs), nil
	}
	var found []string
	for _, r := range repos {
		if strings.EqualFold(filepath.Base(r), arg) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no repo %q under %s", arg, strings.Join(mapTilde(repoRoots), " or "))
	}
	return "", fmt.Errorf("%q names %s; give its path", arg, strings.Join(mapTilde(found), " and "))
}

func mapTilde(paths []string) []string {
	out := make([]string, len(paths))
	for n, p := range paths {
		out[n] = tilde(p)
	}
	return out
}

// resolveAgent takes pi, claude or yolo, or an agent's label.
func resolveAgent(name string) (string, error) {
	switch strings.ToLower(name) {
	case "pi":
		return agentPi, nil
	case "claude":
		return agentClaude, nil
	case "yolo", "claude (yolo)":
		return "Claude (yolo)", nil
	}
	return "", fmt.Errorf("no agent %q: pi, claude or yolo", name)
}

// findPr reads the PR named by repo#n from GitHub, with the checkout the review starts from.
func findPr(key string) (Pr, error) {
	name, num, ok := strings.Cut(key, "#")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || name == "" {
		return Pr{}, fmt.Errorf("--pr %q: want REPO#N, e.g. reports-app#1185", key)
	}
	for full, dir := range checkouts() {
		_, repo, _ := strings.Cut(full, "/")
		if !strings.EqualFold(repo, name) {
			continue
		}
		pr := Pr{Repo: full, Number: n, Checkout: dir}
		v, err := viewPr(pr)
		if err != nil {
			return Pr{}, fmt.Errorf("%s: %w", key, err)
		}
		// The board keys a PR by GitHub's name for the repo, in its case; the checkout map is lower case.
		pr.Key = fmt.Sprintf("%s#%d", githubName(v.URL, repo), n)
		pr.Summary, pr.URL, pr.Branch = v.Title, v.URL, v.HeadRefName
		return pr, nil
	}
	return Pr{}, fmt.Errorf("no GitHub checkout of %s under %s", name, tilde(prRoot))
}

// githubName is the repo's name in a PR's URL, github.com/<owner>/<name>/pull/<n>, else fallback.
func githubName(url, fallback string) string {
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), "/")
	if len(parts) >= 4 && parts[3] == "pull" {
		return parts[2]
	}
	return fallback
}

func runDispatch(argv []string) error {
	if len(argv) > 0 && (argv[0] == "--help" || argv[0] == "-h") {
		fmt.Println(dispatchUsage)
		return nil
	}
	a, err := parseDispatchArgs(argv, os.Stdin)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, dispatchUsage)
	}
	agent := ""
	if a.agent != "" {
		if agent, err = resolveAgent(a.agent); err != nil {
			return err
		}
	}
	progress := func(line string) { fmt.Fprintln(os.Stderr, line) }
	state, repos := loadState(), listRepos()
	repo := ""
	if a.repo != "" {
		if repo, err = resolveRepo(a.repo, repos); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()
	jira := &Jira{log: func(l string) { fmt.Fprintln(os.Stderr, l) }}
	defer jira.close()

	var key string
	var issue Issue
	var pr Pr
	switch {
	case a.pr != "":
		if pr, err = findPr(a.pr); err != nil {
			return err
		}
		key, repo = pr.Key, pr.Checkout
		if a.fix {
			key = fixKey(pr.Key)
		}
	case a.ticket != "":
		key = strings.ToUpper(a.ticket)
		if repo == "" {
			if repo = state.RepoByProject[project(key)]; repo == "" {
				return fmt.Errorf("no repo used for %s yet; pass --repo", project(key))
			}
		}
		progress("Reading " + key + "…")
		list, _, err := jira.issues(ctx, "key = "+key)
		if err == nil && len(list) == 0 {
			err = errors.New("not in Jira")
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		issue = list[0]
	}

	agent = cmp.Or(agent, defaultAgent(state, repo))
	if session := state.Sessions[key]; key != "" && session != "" && !a.again {
		if states, err := agentStates(); err == nil && states[session] != "" {
			return fmt.Errorf("%s's agent session is still open (%s); pass --again to start a second agent", key, states[session])
		}
	}
	_, _ = updateState(func(s *State) {
		if a.ticket != "" {
			s.RepoByProject[project(key)] = repo
		}
		s.AgentByRepo[repo] = agent
	})

	var text string
	switch {
	case a.pr != "":
		text, err = dispatchPr(ctx, jira, PrDispatch{Pr: pr, Agent: agent, Context: a.text, Fix: a.fix, Title: a.title}, progress)
	case a.ticket != "":
		text, err = dispatch(ctx, jira, Dispatch{Issue: issue, Agent: agent, Worktree: !a.noWorktree, Poteto: a.poteto, Repo: repo, Context: a.text}, progress)
	default:
		text, err = dispatchTask(TaskDispatch{Repo: repo, Agent: agent, Worktree: !a.noWorktree, Poteto: a.poteto, Title: a.title, Task: a.text}, progress)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", cmp.Or(key, "task"), err)
	}
	fmt.Println(text)
	return nil
}
