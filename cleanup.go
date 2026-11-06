package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// A cleanupCandidate is a dispatched worktree whose ticket is done or whose PR is closed.
type cleanupCandidate struct {
	Key    string
	Dir    string
	Repo   string // the main checkout that owns the worktree
	Reason string // "done", "merged", "closed"
	Dirty  bool   // uncommitted changes: git refuses to remove it
	Agent  string // the agent's reported state, if its session is still open
}

type cleanupView struct {
	items  []cleanupCandidate
	picked map[int]bool
	cursor int
	busy   bool
}

// findCleanup checks every dispatched worktree. Entries whose directory is gone are dropped from
// the state on the way, since there is nothing left to clean.
func (m *model) findCleanup() tea.Cmd {
	jira, state, agents := m.jira, m.state, m.agents
	m.say("Checking dispatched worktrees…", cSubtext)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
		defer cancel()
		var gone []string
		var out []cleanupCandidate
		var mu sync.Mutex
		var wg sync.WaitGroup
		for key, dir := range state.Dispatched {
			if !exists(dir) {
				gone = append(gone, key)
				continue
			}
			wg.Go(func() {
				c, ok := checkCleanup(ctx, jira, key, dir)
				if !ok {
					return
				}
				if s := agents[state.Sessions[key]]; s != "" {
					c.Agent = s
				}
				mu.Lock()
				out = append(out, c)
				mu.Unlock()
			})
		}
		wg.Wait()
		if len(gone) > 0 {
			_, _ = updateState(func(s *State) {
				for _, k := range gone {
					delete(s.Dispatched, k)
					delete(s.Sessions, k)
				}
			})
		}
		slices.SortFunc(out, func(a, b cleanupCandidate) int { return strings.Compare(a.Dir, b.Dir) })
		return cleanupMsg{out, ctx.Err()}
	}
}

func checkCleanup(ctx context.Context, jira *Jira, key, dir string) (cleanupCandidate, bool) {
	c := cleanupCandidate{Key: key, Dir: dir}
	if _, num, isPr := strings.Cut(key, "#"); isPr {
		// gh finds the GitHub repo from the worktree's remote.
		state, err := sh(dir, "gh", "pr", "view", num, "--json", "state", "-q", ".state")
		state = strings.TrimSpace(state)
		if err != nil || (state != "MERGED" && state != "CLOSED") {
			return c, false
		}
		c.Reason = strings.ToLower(state)
	} else {
		if jira.category(ctx, key) != "Done" {
			return c, false
		}
		c.Reason = "done"
	}
	common, err := sh("", "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return c, false
	}
	c.Repo = filepath.Dir(strings.TrimSpace(common))
	// Only worktrees: a dispatch without one ran in the main checkout, which stays.
	if c.Repo == filepath.Clean(dir) {
		return c, false
	}
	dirty, err := sh("", "git", "-C", dir, "status", "--porcelain")
	c.Dirty = err != nil || strings.TrimSpace(dirty) != ""
	return c, true
}

func (m *model) openCleanup(msg cleanupMsg) {
	m.state = loadState()
	if msg.err != nil {
		m.say("Checking worktrees: "+msg.err.Error(), cRed)
		return
	}
	if len(msg.candidates) == 0 {
		m.say("No worktrees to clean up: every dispatched ticket and PR is still open", cGreen)
		return
	}
	v := &cleanupView{items: msg.candidates, picked: map[int]bool{}}
	for n, c := range v.items {
		// A running agent may still be writing there; leave it unless picked by hand.
		v.picked[n] = !c.Dirty && !agentActive(c.Agent)
	}
	m.cleanup = v
	m.say("", cOverlay)
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
	return func() tea.Msg {
		var removed, errs []string
		for _, c := range picked {
			// Without --force git refuses a worktree with changes; the branch and its commits stay.
			if _, err := sh("", "git", "-C", c.Repo, "worktree", "remove", c.Dir); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", tilde(c.Dir), firstLine(err.Error())))
				continue
			}
			removed = append(removed, c.Key)
		}
		_, _ = updateState(func(s *State) {
			for _, k := range removed {
				delete(s.Dispatched, k)
				delete(s.Sessions, k)
			}
		})
		return cleanedMsg{removed, errs}
	}
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
		bold("Worktrees whose ticket is done or PR is closed"),
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
	return append(out, fg(cOverlay, "↑↓ pick · space toggle · a all/none · enter remove (branches stay) · esc back"))
}
