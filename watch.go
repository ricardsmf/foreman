package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
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
	// On names what about a PR wakes the watcher: "commit" (its head moved), "checks" (a
	// check newly failing), "conflict", "comment" (a comment or review by someone else) and
	// "threads" (a review thread opened or resolved). Without it: commit, checks, conflict, comment.
	On []string `json:"on,omitempty"`
	// Batch is how many PRs a pass takes at most. A wake then counts only that
	// many changed PRs as seen, lowest number first, and the rest wake it again once it rests.
	Batch int `json:"batch,omitempty"`
	// At is when the board starts a pass by the clock: "17:45" every day, or "mon-fri 17:45". The
	// board types Prompt then, so such a watcher doesn't loop. With PRs too, it is a sweep: a pass
	// that takes whatever the passes its PRs woke left undone, such as a review that failed.
	At string `json:"at,omitempty"`
}

// Mine, like the other defaults; "watchers" in the config file replaces the list. fld-pr-watch's
// searches are the two fld-watch's own pass runs, my PRs and the PRs that request my review, and it takes three
// PRs a pass. --once: without it the skill starts a ten-minute loop of its own. The board only
// says when to look: which PRs are due, and what for, is the skill's to work out, by rules that
// change with it. A new head is a review due, and a thread resolved may finish one.
var watchers = []Watcher{{
	Name:   "fld-pr-watch",
	Dir:    filepath.Join(home, "work", "code", "reports-app-watch"),
	Prompt: "/fld-watch --once",
	PRs: []string{
		"author:@me is:open draft:false -label:agent-review-done -base:main",
		"user-review-requested:@me is:open draft:false -base:main",
	},
	On:    []string{"commit", "threads"},
	Batch: 3,
	// A review that failed is only taken again by a later pass: one each morning, at least.
	At: "mon-fri 9:00",
}, {
	// My end-of-day update, each weekday. Its directory allows what gathers the day (gh, the Jira
	// search, the Slack search) and not the Slack send, so the pass ends on that prompt, with
	// the message in it, for me to send or not.
	Name:   "eod",
	Dir:    filepath.Join(home, ".config", "foreman", "eod"),
	Prompt: "/eod",
	At:     "mon-fri 17:30",
}}

// watcherCommand runs Claude Code in manualMode: the session reads text other people wrote, so
// only what its checkout's allowlist names may run unasked, and no prompt can switch it to auto
// mode. It gets no --mcp-config: it needs none of those servers. With a prompt it starts a pass;
// without, it waits at its own prompt for the poll to wake it. On macOS Claude Code leaves Glob and
// Grep out unless --allowedTools names them, and watcherRules sends the pass to them instead of
// find, whose -exec and -delete the allowlist can't fence. The rules stop at the checkout: a bare
// `Grep` would search any file on disk unasked, where Read asks. --allowedTools takes a list, so
// it goes last, after the prompt.
func watcherCommand(prompt string) []string {
	cmd := append([]string{which("claude")}, manualMode...)
	if prompt != "" {
		cmd = append(cmd, prompt)
	}
	return append(cmd, "--allowedTools", "Glob(./**)", "Grep(./**)")
}

// watcherRules keep a pass's tool calls inside its checkout's allowlist. A permission rule matches
// one plain command, so a loop, a variable or a chain of commands asks even when each part is
// allowed. Rex's startup hook, run again by each /clear, asks the session to rename itself.
const watcherRules = "Each Bash call is one command the skill names, with its values written out: no loop, variable, `;`, `&&`, `|`, redirect, `cd` or `echo`, since anything the allowlist doesn't match waits for me to approve it. Read, search and list files with Read, Grep and Glob. Run nothing the skill doesn't name, and leave the Rex session's name as it is."

// passPrompt starts a pass: the watcher's prompt, then watcherRules. It names no PR: the pass
// finds and judges them by its skill's rules, which the board doesn't hold.
func passPrompt(w Watcher) string {
	return w.Prompt + " " + watcherRules
}

// startWatcher starts the watcher in a background Rex session named after it, running a pass on
// prompt or, with none, waiting for its first wake, and returns the session's ID.
func startWatcher(w Watcher, prompt string) (string, error) {
	if !exists(w.Dir) {
		return "", fmt.Errorf("%s: its checkout %s doesn't exist", w.Name, tilde(w.Dir))
	}
	id, _, err := newSession(w.Name, w.Dir, agentClaude, watcherCommand(prompt))
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

// startWatchers starts each watcher that has no open session and that I didn't stop, and returns
// the ones it started. A session I started by hand under the watcher's name, or in its checkout,
// counts as it, stopped or not. Every watcher's session is recorded by ID. One foreman at a time:
// the board and foreman watch would each find a watcher gone and start it, and two sessions
// would review every PR twice.
func startWatchers() (started []string, err error) {
	unlock, err := lockFile(stateFile + ".watchers.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	sessions, err := agentSessions()
	if err != nil {
		return nil, err
	}
	state := loadState()
	recorded := state.WatcherSessions
	ids := map[string]string{}
	baselines := map[string]map[string]string{}
	var failed []string
	for _, w := range watchers {
		if s, ok := findWatcher(sessions, w, recorded[w.Name]); ok {
			ids[w.Name] = s.ID
			continue
		}
		if state.WatcherStopped[w.Name] {
			continue
		}
		// The pass its prompt starts covers the PRs as they are now, so the poll wakes it only for
		// what changes after. Read before the start: a change in between wakes it once more, not never.
		// A watcher with only a schedule starts without a pass: its pass belongs to its time. With a
		// batch the pass takes only some of what changed, so the rest stays unseen and wakes it again.
		var baseline map[string]string
		pass := w.At == "" || len(w.PRs) > 0
		if len(w.PRs) > 0 {
			var e error
			baseline, e = prSignatures(w)
			if e == nil {
				baseline, _ = seenAfter(state.WatcherPrs[w.Name], baseline, w.Batch)
			}
		}
		prompt := ""
		if pass {
			prompt = passPrompt(w)
		}
		id, e := startWatcher(w, prompt)
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
		for name := range ids {
			delete(s.WatcherStopped, name)
		}
	}); e != nil {
		failed = append(failed, "recording their sessions: "+e.Error())
	}
	if len(failed) > 0 {
		err = fmt.Errorf("watcher %s", strings.Join(failed, "; "))
	}
	return started, err
}

// closeSession closes an agent's Rex session. A watcher is marked stopped first: foreman watch
// starts a watcher whose session is gone, and this one is gone because I said so.
func closeSession(session, key string, watcher bool) error {
	if watcher {
		if _, err := updateState(func(s *State) { s.WatcherStopped[key] = true }); err != nil {
			return err
		}
	}
	return rex.kill(session)
}

// restartWatcher starts a watcher from the queue, taking back the stop that keeps startWatchers
// off it.
func restartWatcher(name string) error {
	if _, err := updateState(func(s *State) { delete(s.WatcherStopped, name) }); err != nil {
		return err
	}
	_, err := startWatchers()
	return err
}

// watcherItem is the watcher's line in the queue: with the agents that need me when it waits on a
// permission prompt or failed, else by whether it works, waits for its next pass or isn't running.
func watcherItem(w Watcher, sessions []rexSession, recorded string) queueItem {
	it := queueItem{Kind: agentItem, Watcher: true, Polled: len(w.PRs) > 0, At: w.At,
		Key: w.Name, Title: w.Prompt, Dir: w.Dir, Group: closed}
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

// pollWatchers runs for the board's life: it polls each watcher with PR searches or a schedule now
// and every watchPoll, and wakes the ones whose PRs changed or whose time has come. note shows a
// line on the board's status bar.
func pollWatchers(note func(text string, c Color)) {
	if !slices.ContainsFunc(watchers, Watcher.woken) {
		return
	}
	lastErr := map[string]string{}
	for {
		if sessions, err := agentSessions(); err == nil {
			wakeWatchers(sessions, loadState().WatcherSessions, note, lastErr)
		}
		time.Sleep(watchPoll)
	}
}

// woken is whether the board wakes the watcher, rather than leaving it to loop.
func (w Watcher) woken() bool { return len(w.PRs) > 0 || w.At != "" }

// wakeWatchers polls each watcher the board wakes, once, and notes the wakes and the failures.
// lastErr holds each watcher's last failure: the same one every two minutes would bury whatever
// else the status bar says.
func wakeWatchers(sessions []rexSession, recorded map[string]string, note func(text string, c Color), lastErr map[string]string) {
	for _, w := range watchers {
		if !w.woken() {
			continue
		}
		woke, e := pollOne(w, sessions, recorded[w.Name])
		switch {
		case e != nil && e.Error() != lastErr[w.Name]:
			note(w.Name+": "+firstLine(e.Error()), cRed)
		case woke != "":
			note(woke, cSubtext)
		}
		lastErr[w.Name] = ""
		if e != nil {
			lastErr[w.Name] = e.Error()
		}
	}
}

// pollOne polls a watcher by its schedule, then by its PRs, and returns the status line of a wake.
func pollOne(w Watcher, sessions []rexSession, recorded string) (string, error) {
	if w.At != "" {
		at, err := pollSchedule(w, sessions, recorded, time.Now())
		if err != nil {
			return "", err
		}
		if !at.IsZero() {
			return fmt.Sprintf("Woke %s for %s", w.Name, at.Format("15:04")), nil
		}
	}
	if len(w.PRs) == 0 {
		return "", nil
	}
	changed, err := pollWatcher(w, sessions, recorded)
	if err != nil || len(changed) == 0 {
		return "", err
	}
	return fmt.Sprintf("Woke %s: %s changed", w.Name, strings.Join(changed, ", ")), nil
}

// pollSchedule starts the watcher's pass when its time has come, and returns that time. A watcher
// that isn't running or is busy is left for the next poll, as pollWatcher leaves it: the time is
// only recorded on a wake, so the pass starts once the watcher rests, if that is the same day.
func pollSchedule(w Watcher, sessions []rexSession, recorded string, now time.Time) (time.Time, error) {
	s, ok := findWatcher(sessions, w, recorded)
	if !ok || !wakeable(s.State) {
		return time.Time{}, nil
	}
	sc, err := parseAt(w.At)
	if err != nil {
		return time.Time{}, err
	}
	return wakeIfDue(w.Name, sc, now, func() error { return wakeWatcher(s, passPrompt(w)) })
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
	return wakeIfChanged(w.Name, now, w.Batch, func() error { return wakeWatcher(s, passPrompt(w)) })
}

// wakeWatcher starts a pass in the watcher's pane, on an empty conversation: a pass reads
// everything it needs from GitHub, and the PRs of the passes before would be paid for again on
// every request of this one, and leave their authors' text in front of the model.
func wakeWatcher(s rexSession, prompt string) error {
	if err := sendToAgent(s.ID, s.Block, "/clear"); err != nil {
		return err
	}
	time.Sleep(sendSettle)
	return sendToAgent(s.ID, s.Block, prompt)
}

// wakeable is whether the watcher sits at its prompt, so typing into it starts a pass. Working or
// blocked on a permission prompt, it is busy; "open" means its program reports no state, so it may
// still be starting, or not be Claude Code any more.
func wakeable(state string) bool {
	return state == "idle" || state == "done" || state == "error"
}

// wakeIfChanged records the signatures and calls wake when a PR changed since the last wake. The
// record comes first, under the state lock, so two boards polling at once wake the watcher once;
// a failed wake puts the old record back for the next poll to retry. With a batch, only the PRs
// seenAfter counts as seen are recorded and returned.
func wakeIfChanged(name string, now map[string]string, batch int, wake func() error) ([]string, error) {
	var changed []string
	var old, seen map[string]string
	if _, err := updateState(func(s *State) {
		old = s.WatcherPrs[name]
		if seen, changed = seenAfter(old, now, batch); len(changed) > 0 {
			s.WatcherPrs[name] = seen
		}
	}); err != nil || len(changed) == 0 {
		return nil, err
	}
	if err := wake(); err != nil {
		_, _ = updateState(func(s *State) {
			if maps.Equal(s.WatcherPrs[name], seen) {
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

// seenAfter is what a wake records of the PRs as they are now, and the changed PRs it hands the
// pass, sorted. Without a batch that is all of them. With one, a pass takes at most that many, so
// only the batch lowest-numbered changed PRs count as seen: the others keep the signature
// recorded before, or none, and so still differ at the next poll.
func seenAfter(old, now map[string]string, batch int) (map[string]string, []string) {
	changed := changedPrs(old, now)
	if batch <= 0 || len(changed) <= batch {
		return now, changed
	}
	number := func(key string) int {
		_, n, _ := strings.Cut(key, "#")
		num, _ := strconv.Atoi(n)
		return num
	}
	slices.SortFunc(changed, func(a, b string) int { return cmp.Or(number(a)-number(b), strings.Compare(a, b)) })
	seen := maps.Clone(now)
	for _, k := range changed[batch:] {
		if sig, ok := old[k]; ok {
			seen[k] = sig
		} else {
			delete(seen, k)
		}
	}
	handed := slices.Sorted(slices.Values(changed[:batch]))
	return seen, handed
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
      reviewThreads(first: 100) { nodes { isResolved } }
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
	ReviewThreads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
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

// wakeParts are what a watcher's `on` may name, in the order a signature lists them, and
// defaultParts the ones a watcher without it is woken on.
var (
	wakeParts    = []string{"commit", "checks", "conflict", "comment", "threads"}
	defaultParts = wakeParts[:4]
)

// prSignature is what the watcher last saw of the PR, of the parts it is woken on: its head
// commit, its failing checks, whether it conflicts, the newest comment or review by someone other
// than me, and its review threads, open and in all. Readable, so state.json shows what woke the
// watcher.
func prSignature(p watchedPr, me string, on []string) string {
	if len(on) == 0 {
		on = defaultParts
	}
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
	open := 0
	for _, t := range p.ReviewThreads.Nodes {
		if !t.IsResolved {
			open++
		}
	}
	part := map[string]string{
		"commit":   "head=" + p.HeadRefOid,
		"checks":   "failing=" + failingChecks(checks),
		"conflict": "merge=" + merge,
		"comment":  "comment=" + id,
		"threads":  fmt.Sprintf("threads=%d/%d", open, len(p.ReviewThreads.Nodes)),
	}
	var sig []string
	for _, name := range wakeParts {
		if slices.Contains(on, name) {
			sig = append(sig, part[name])
		}
	}
	return strings.Join(sig, " ")
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
				sigs[p.key()] = prSignature(p, me, w.On)
			}
		}
	}
	return sigs, nil
}
