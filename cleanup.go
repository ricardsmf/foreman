package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// A cleanupCandidate is a dispatch whose ticket is done, whose PR is closed, or a task whose
// agent's session is closed.
type cleanupCandidate struct {
	Key     string
	Dir     string
	Repo    string // the main checkout that owns the worktree
	Main    bool   // dispatched without a worktree: Dir is the main checkout, which stays
	Reason  string // "done", "merged", "closed", "session closed"
	Dirty   bool   // uncommitted changes: git refuses to remove it
	Agent   string // the agent's reported state, if its session is still open
	Session string // the agent's Rex session label
}

type cleanupView struct {
	items  []cleanupCandidate
	picked map[int]bool
	cursor int
	busy   bool
}

// findCleanup checks every dispatch. Those whose directory is gone are discarded on the way: the
// worktree was removed by hand, and nothing lists their report any more.
func (m *model) findCleanup() tea.Cmd {
	jira, state, agents := m.jira, m.state, m.agents
	m.say("Checking dispatched worktrees…", cSubtext)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
		defer cancel()
		return cleanupCandidates(ctx, jira, state, agents)
	}
}

// cleanupCandidates checks every dispatch against Jira, GitHub and its session. agents maps Rex
// session IDs and labels to their reported states.
func cleanupCandidates(ctx context.Context, jira *Jira, state State, agents map[string]string) cleanupMsg {
	var gone, failed []string
	var out []cleanupCandidate
	var mu sync.Mutex
	var wg sync.WaitGroup
	// Each check is a Jira or GitHub call plus a few git commands; a long-lived board can have
	// dozens, and all at once trip rate limits.
	slots := make(chan struct{}, cleanupWorkers)
	for key, dir := range state.Dispatched {
		if !exists(dir) {
			gone = append(gone, key)
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			c, ok, err := checkCleanup(ctx, jira, key, dir, agents[state.Sessions[key]] != "")
			if err != nil {
				mu.Lock()
				failed = append(failed, key+": "+firstLine(err.Error()))
				mu.Unlock()
			}
			if !ok {
				return
			}
			c.Session = state.Sessions[key]
			if s := agents[c.Session]; s != "" {
				c.Agent = s
			}
			mu.Lock()
			out = append(out, c)
			mu.Unlock()
		})
	}
	wg.Wait()
	discard(gone)
	slices.SortFunc(out, func(a, b cleanupCandidate) int { return strings.Compare(a.Dir, b.Dir) })
	slices.Sort(failed)
	return cleanupMsg{out, failed, ctx.Err()}
}

// defaultPicked is whether cleanup ticks the candidate unasked: a running agent may still be
// writing there, and git refuses a worktree with changes anyway.
func defaultPicked(c cleanupCandidate) bool { return !c.Dirty && !agentActive(c.Agent) }

const cleanupWorkers = 8

// discard forgets the dispatches and removes their reports and evidence.
func discard(keys []string) {
	if len(keys) == 0 {
		return
	}
	_, _ = updateState(func(s *State) { forget(s, keys) })
	for _, k := range keys {
		_ = os.RemoveAll(evidenceDir(k))
	}
}

// forget drops keys from the state with their dismissals.
func forget(s *State, keys []string) {
	for _, k := range keys {
		delete(s.Dispatched, k)
		delete(s.Sessions, k)
		delete(s.Titles, k)
		delete(s.StartedFrom, k)
		delete(s.Questions, k)
		delete(s.Dismissed, k)
		delete(s.Hidden, k)
	}
}

// checkCleanup is the worktree as a candidate, when its ticket is done, its PR closed, or, for a
// task, its agent's session closed. An error means it couldn't tell: a failed Jira or GitHub call
// mustn't read as "still open".
func checkCleanup(ctx context.Context, jira *Jira, key, dir string, sessionOpen bool) (cleanupCandidate, bool, error) {
	c := cleanupCandidate{Key: key, Dir: dir}
	if isTask(key) {
		// Nothing outside says a task is done; closing its session is how I say so.
		if sessionOpen {
			return c, false, nil
		}
		c.Reason = "session closed"
	} else if _, num, isPr := strings.Cut(key, "#"); isPr {
		// gh finds the GitHub repo from the worktree's remote.
		state, err := sh(dir, "gh", "pr", "view", num, "--json", "state", "-q", ".state")
		if err != nil {
			return c, false, err
		}
		state = strings.TrimSpace(state)
		if state != "MERGED" && state != "CLOSED" {
			return c, false, nil
		}
		c.Reason = strings.ToLower(state)
	} else {
		category, err := jira.category(ctx, key)
		if err != nil {
			return c, false, err
		}
		if category != "Done" {
			return c, false, nil
		}
		c.Reason = "done"
	}
	var err error
	if c.Repo, c.Main, err = owner(dir); err != nil {
		return c, false, err
	}
	// A dispatch without a worktree ran in the main checkout, which stays whatever it holds.
	if c.Main {
		return c, true, nil
	}
	dirty, err := sh("", "git", "-C", dir, "status", "--porcelain")
	c.Dirty = err != nil || strings.TrimSpace(dirty) != ""
	return c, true, nil
}

// owner is the main checkout that owns dir, and whether dir is that checkout itself rather than a
// worktree of it.
func owner(dir string) (repo string, main bool, err error) {
	out, err := sh("", "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", false, err
	}
	dirs := strings.Split(strings.TrimSpace(out), "\n")
	if len(dirs) != 2 {
		return "", false, fmt.Errorf("git rev-parse in %s: %q", tilde(dir), out)
	}
	// A worktree's git directory is its own, under the main checkout's; the main checkout's is the common one.
	return filepath.Dir(dirs[1]), dirs[0] == dirs[1], nil
}

func (m *model) openCleanup(msg cleanupMsg) {
	m.state = loadState()
	if msg.err != nil {
		m.say("Checking worktrees: "+msg.err.Error(), cRed)
		return
	}
	unchecked := ""
	if n := len(msg.failed); n > 0 {
		unchecked = fmt.Sprintf("Couldn't check %d of the dispatched worktrees: %s", n, msg.failed[0])
	}
	if len(msg.candidates) == 0 {
		if unchecked != "" {
			m.say(unchecked, cRed)
		} else {
			m.say("No worktrees to clean up: every dispatched ticket, PR and task is still open", cGreen)
		}
		return
	}
	v := &cleanupView{items: msg.candidates, picked: map[int]bool{}}
	for n, c := range v.items {
		v.picked[n] = defaultPicked(c)
	}
	m.cleanup = v
	m.say(unchecked, cRed)
}

func agentActive(state string) bool { return state == "working" || state == "blocked" }

func (m *model) removeWorktrees() tea.Cmd {
	v := m.cleanup
	var picked []cleanupCandidate
	for n, c := range v.items {
		if v.picked[n] {
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		return nil
	}
	v.busy = true
	return func() tea.Msg { return removeCandidates(picked) }
}

// removeCandidates removes the candidates' worktrees, without --force, closes their agents'
// sessions and forgets them with their reports.
func removeCandidates(picked []cleanupCandidate) cleanedMsg {
	var done, errs []string
	removed, forgotten := 0, 0
	for _, c := range picked {
		if c.Main {
			forgotten++
		} else {
			// Without --force git refuses a worktree with changes; the branch and its commits stay.
			if _, err := sh("", "git", "-C", c.Repo, "worktree", "remove", c.Dir); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", tilde(c.Dir), firstLine(err.Error())))
				continue
			}
			removed++
		}
		done = append(done, c.Key)
		// Its agent's session has nothing left to work on. A failure to close it isn't one
		// worth reporting: the worktree, which was the point, is gone.
		if c.Session != "" && c.Agent != "" {
			_, _ = sh("", "rex", "kill", c.Session)
		}
	}
	discard(done)
	return cleanedMsg{removed, forgotten, errs}
}

func (m *model) cleanupInput(msg tea.KeyPressMsg) tea.Cmd {
	v := m.cleanup
	if v.busy {
		return nil
	}
	switch msg.String() {
	case "esc":
		m.cleanup = nil
	case "ctrl+c":
		return tea.Quit
	case "up", "ctrl+p":
		v.cursor = max(0, v.cursor-1)
	case "down", "ctrl+n":
		v.cursor = min(len(v.items)-1, v.cursor+1)
	case "space":
		v.picked[v.cursor] = !v.picked[v.cursor]
	case "a":
		all := true
		for n := range v.items {
			all = all && v.picked[n]
		}
		for n := range v.items {
			v.picked[n] = !all
		}
	case "enter":
		return m.removeWorktrees()
	}
	return nil
}

func (m *model) renderCleanup(width, h int) []string {
	v := m.cleanup
	out := []string{m.header("clean up worktrees"),
		bold("Dispatches whose ticket is done, PR is closed, or task's session is closed"),
		fg(cMauve, strings.Repeat("─", width))}
	for n, c := range v.items {
		mark := " "
		if n == v.cursor {
			mark = fg(cMauve, "▌")
		}
		box := fg(cOverlay, "[ ]")
		if v.picked[n] {
			box = fg(cGreen, "[x]")
		}
		var notes []string
		notes = append(notes, c.Reason)
		if c.Main {
			notes = append(notes, "checkout stays")
		}
		if c.Dirty {
			notes = append(notes, fg(cRed, "uncommitted changes"))
		}
		if c.Agent != "" {
			color := cOverlay
			if agentActive(c.Agent) {
				color = cYellow
			}
			notes = append(notes, fg(color, "agent "+c.Agent))
		}
		name := tilde(c.Dir)
		if n == v.cursor {
			name = bold(name)
		}
		out = append(out, mark+box+" "+fit(name, max(10, width/2))+"  "+fg(cSubtext, c.Key)+"  "+fg(cOverlay, strings.Join(notes, " · ")))
	}
	for len(out) < h-1 {
		out = append(out, "")
	}
	out = out[:max(0, h-1)]
	if v.busy {
		return append(out, fg(cOverlay, "removing…"))
	}
	return append(out, fg(cOverlay, "↑↓ pick · space toggle · a all/none · enter remove (branches stay; agent sessions close) · esc back"))
}
