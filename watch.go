package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A Watcher is a Claude Code session that runs one of a repo's skills, such as `/fld-watch`: the
// board starts it, wakes it when a PR it covers changes, and the queue shows whether it runs or
// waits on me.
type Watcher struct {
	// Name is its Rex session's label and its key in the queue.
	Name string `json:"name"`
	// Dir is the checkout it runs in: one nobody works in, since the session reads other people's code.
	Dir    string `json:"dir"`
	Prompt string `json:"prompt"`
	// PRs are GitHub searches, scoped to Dir's origin repo, for the PRs the watcher covers. With
	// them the board polls those PRs and types Prompt into the watcher when one changed; without,
	// the watcher only gets Prompt at start, so it must keep itself going (`/loop 10m …`).
	PRs []string `json:"prs,omitempty"`
}

// Mine, like the other defaults; "watchers" in the config file replaces the list. The searches are
// the two fld-watch's own pass runs: PRs that request my review, and PRs I reviewed, for new commits.
var watchers = []Watcher{{
	Name:   "fld-pr-watch",
	Dir:    filepath.Join(home, "work", "code", "reports-app-watch"),
	Prompt: "/fld-watch",
	PRs: []string{
		"user-review-requested:@me is:open draft:false base:develop",
		"reviewed-by:@me is:open draft:false base:develop",
	},
}}

// watcherCommand runs Claude Code on the watcher's prompt, in manualMode: the session reads text
// other people wrote, so only what its checkout's allowlist names may run unasked, and no prompt
// can switch it to auto mode. It gets no --mcp-config: it needs none of those servers.
func watcherCommand(w Watcher) []string {
	return append(append([]string{which("claude")}, manualMode...), w.Prompt)
}

// startWatcher starts the watcher in a background Rex session named after it, and returns the
// session's ID.
func startWatcher(w Watcher) (string, error) {
	if !exists(w.Dir) {
		return "", fmt.Errorf("%s: its checkout %s doesn't exist", w.Name, tilde(w.Dir))
	}
	id, _, err := newSession(w.Name, w.Dir, agentClaude, watcherCommand(w))
	if err != nil {
		return "", fmt.Errorf("%s: %w", w.Name, err)
	}
	return id, nil
}

// findWatcher is the watcher's open session: the recorded one, else one named after it, else one
// running Claude Code in its checkout. Claude renames its session when Rex's startup hook says so,
// and a watcher that lost its name must not be started twice: two would review every PR twice.
func findWatcher(sessions []rexSession, w Watcher, recorded string) (rexSession, bool) {
	if s, ok := findSession(sessions, w.Name, recorded); ok {
		return s, true
	}
	for _, s := range sessions {
		if s.runsClaudeIn(w.Dir) {
			return s.onClaudeIn(w.Dir), true
		}
	}
	return rexSession{}, false
}

// startWatchers starts each watcher that has no open session, and returns what it did, one line
// each. A session I started by hand under the watcher's name, or in its checkout, counts as it.
// Every watcher's session is recorded by ID.
func startWatchers() (started []string, err error) {
	sessions, err := agentSessions()
	if err != nil {
		return nil, err
	}
	recorded := loadState().WatcherSessions
	ids := map[string]string{}
	baselines := map[string]map[string]string{}
	var failed []string
	for _, w := range watchers {
		if s, ok := findWatcher(sessions, w, recorded[w.Name]); ok {
			ids[w.Name] = s.ID
			continue
		}
		// The pass its prompt starts covers the PRs as they are now, so the poll wakes it only for
		// what changes after. Read before the start: a change in between wakes it once more, not never.
		var baseline map[string]string
		if len(w.PRs) > 0 {
			baseline, _ = prSignatures(w)
		}
		id, e := startWatcher(w)
		if e != nil {
			failed = append(failed, e.Error())
			continue
		}
		ids[w.Name] = id
		if baseline != nil {
			baselines[w.Name] = baseline
		}
		started = append(started, w.Name)
	}
	if _, e := updateState(func(s *State) {
		maps.Copy(s.WatcherSessions, ids)
		maps.Copy(s.WatcherPrs, baselines)
	}); e != nil {
		failed = append(failed, "recording their sessions: "+e.Error())
	}
	if len(failed) > 0 {
		err = fmt.Errorf("watcher %s", strings.Join(failed, "; "))
	}
	return started, err
}

// watcherItem is the watcher's line in the queue: with the agents that need me when it waits on a
// permission prompt or failed, else by whether it works, waits for its next pass or isn't running.
func watcherItem(w Watcher, sessions []rexSession, recorded string) queueItem {
	it := queueItem{Kind: agentItem, Watcher: true, Polled: len(w.PRs) > 0, Key: w.Name, Title: w.Prompt, Dir: w.Dir, Group: closed}
	if rs, ok := findWatcher(sessions, w, recorded); ok {
		it.on(rs)
		switch {
		case it.urgent():
			it.Group = needsYou
		case it.Agent == "working":
			it.Group = working
		default:
			it.Group = idle
		}
	}
	it.Sig = it.Agent
	return it
}

// watchPoll is how often the board reads the watchers' PRs, as t3code's PR watch does: a few
// GraphQL requests each time, instead of a model turn every ten minutes.
const watchPoll = 2 * time.Minute

// pollWatchers runs for the board's life: it polls each watcher with PR searches now and every
// watchPoll, and wakes the ones whose PRs changed. note shows a line on the board's status bar.
func pollWatchers(note func(text string, c Color)) {
	if !slices.ContainsFunc(watchers, func(w Watcher) bool { return len(w.PRs) > 0 }) {
		return
	}
	// The same failure every two minutes would bury whatever else the status bar says.
	lastErr := map[string]string{}
	for {
		sessions, err := agentSessions()
		recorded := loadState().WatcherSessions
		for _, w := range watchers {
			if len(w.PRs) == 0 || err != nil {
				continue
			}
			changed, e := pollWatcher(w, sessions, recorded[w.Name])
			switch {
			case e != nil && e.Error() != lastErr[w.Name]:
				note(w.Name+": "+firstLine(e.Error()), cRed)
			case len(changed) > 0:
				note(fmt.Sprintf("Woke %s: %s changed", w.Name, strings.Join(changed, ", ")), cSubtext)
			}
			lastErr[w.Name] = ""
			if e != nil {
				lastErr[w.Name] = e.Error()
			}
		}
		time.Sleep(watchPoll)
	}
}

// pollWatcher wakes the watcher when one of its PRs changed since its last wake, and returns those
// PRs. A watcher that isn't running is left so: the poll doesn't restart one I stopped. A busy one
// is left for the next poll, which still sees the change: signatures are only recorded on a wake.
func pollWatcher(w Watcher, sessions []rexSession, recorded string) ([]string, error) {
	s, ok := findWatcher(sessions, w, recorded)
	if !ok || !wakeable(s.State) {
		return nil, nil
	}
	now, err := prSignatures(w)
	if err != nil {
		return nil, err
	}
	return wakeIfChanged(w.Name, now, func() error { return sendToAgent(s.ID, s.Block, w.Prompt) })
}

// wakeable is whether the watcher sits at its prompt, so typing into it starts a pass. Working or
// blocked on a permission prompt, it is busy; "open" means its program reports no state, so it may
// still be starting, or not be Claude Code any more.
func wakeable(state string) bool {
	return state == "idle" || state == "done" || state == "error"
}

// wakeIfChanged records the signatures and calls wake when a PR changed since the last wake. The
// record comes first, under the state lock, so two boards polling at once wake the watcher once;
// a failed wake puts the old record back for the next poll to retry.
func wakeIfChanged(name string, now map[string]string, wake func() error) ([]string, error) {
	var changed []string
	var old map[string]string
	if _, err := updateState(func(s *State) {
		old = s.WatcherPrs[name]
		if changed = changedPrs(old, now); len(changed) > 0 {
			s.WatcherPrs[name] = now
		}
	}); err != nil || len(changed) == 0 {
		return nil, err
	}
	if err := wake(); err != nil {
		_, _ = updateState(func(s *State) {
			if maps.Equal(s.WatcherPrs[name], now) {
				if old == nil {
					delete(s.WatcherPrs, name)
				} else {
					s.WatcherPrs[name] = old
				}
			}
		})
		return nil, fmt.Errorf("waking it: %w", err)
	}
	return changed, nil
}

// changedPrs lists the PRs, sorted, whose signature differs from the last wake's, PRs new to the
// searches included. A PR gone from them (merged, closed, no longer requested) gives the watcher
// nothing to do.
func changedPrs(last, now map[string]string) []string {
	var changed []string
	for k, sig := range now {
		if last[k] != sig {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

// One page of a search, with what a PR's signature is made of. Reviews carry the inline comments
// and their replies, so with the conversation's comments they hold every comment on the PR.
const watchQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 50) {
    nodes { ... on PullRequest {
      number headRefOid mergeable repository { nameWithOwner }
      commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
        ... on CheckRun { name status conclusion } ... on StatusContext { context state }
      } } } } } }
      comments(last: 20) { nodes { id author { login } createdAt } }
      reviews(last: 20) { nodes { id author { login } submittedAt } }
    } }
  }
}`

type watchedPr struct {
	Number     int    `json:"number"`
	HeadRefOid string `json:"headRefOid"`
	Mergeable  string `json:"mergeable"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []prCheck `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Comments struct {
		Nodes []prComment `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		Nodes []prComment `json:"nodes"`
	} `json:"reviews"`
}

// A prCheck is a check run (name, status, conclusion) or a commit status (context, state).
type prCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

// A prComment is a conversation comment (createdAt) or a review (submittedAt).
type prComment struct {
	ID     string `json:"id"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	CreatedAt   string `json:"createdAt"`
	SubmittedAt string `json:"submittedAt"`
}

func (p watchedPr) key() string {
	return strings.ToLower(fmt.Sprintf("%s#%d", p.Repository.NameWithOwner, p.Number))
}

// prSignature is what the watcher last saw of the PR: its head commit, its failing checks,
// whether it conflicts, and the newest comment or review by someone other than me. Readable, so
// state.json shows what woke the watcher.
func prSignature(p watchedPr, me string) string {
	var checks []prCheck
	if n := p.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		checks = n[0].Commit.StatusCheckRollup.Contexts.Nodes
	}
	// GitHub works out whether a PR conflicts lazily, reporting UNKNOWN meanwhile; only a conflict
	// counts, or the watcher would wake each time it flips.
	merge := "ok"
	if p.Mergeable == "CONFLICTING" {
		merge = "conflict"
	}
	newest, id := "", "none"
	for _, c := range append(slices.Clone(p.Comments.Nodes), p.Reviews.Nodes...) {
		at := cmp.Or(c.CreatedAt, c.SubmittedAt)
		// A pending review (no time) is a draft only its author sees.
		if at == "" || (c.Author != nil && strings.EqualFold(c.Author.Login, me)) {
			continue
		}
		if at > newest {
			newest, id = at, c.ID
		}
	}
	return fmt.Sprintf("head=%s failing=%s merge=%s comment=%s", p.HeadRefOid, failingChecks(checks), merge, id)
}

// failingChecks names the checks that failed, sorted and once each. Only failures count, as in
// t3code: a check queuing, running or passing gives the watcher nothing new, and a new commit
// already changes the head.
func failingChecks(checks []prCheck) string {
	var out []string
	for _, c := range checks {
		switch strings.ToUpper(cmp.Or(c.Conclusion, c.State, c.Status)) {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			if name := cmp.Or(c.Name, c.Context, "?"); !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// prSignatures reads the watcher's PRs, by `owner/name#n`, with their signatures: the union of its
// searches, in the GitHub repo its checkout's origin points at.
func prSignatures(w Watcher) (map[string]string, error) {
	me, err := ghLogin()
	if err != nil {
		return nil, err
	}
	url, err := sh("", "git", "-C", w.Dir, "remote", "get-url", "origin")
	if err != nil {
		return nil, err
	}
	m := githubRemote.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return nil, fmt.Errorf("%s's origin isn't on GitHub", tilde(w.Dir))
	}
	sigs := map[string]string{}
	for _, search := range w.PRs {
		out, err := sh("", "gh", "api", "graphql", "-f", "query="+watchQuery, "-f", "q=repo:"+m[1]+" is:pr "+search)
		if err != nil {
			return nil, err
		}
		var res struct {
			Data struct {
				Search struct {
					Nodes []watchedPr `json:"nodes"`
				} `json:"search"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			return nil, err
		}
		for _, p := range res.Data.Search.Nodes {
			if p.Number != 0 {
				sigs[p.key()] = prSignature(p, me)
			}
		}
	}
	return sigs, nil
}
