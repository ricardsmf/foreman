package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
)

// A Watcher is a Claude Code session that keeps running one of a repo's skills, such as
// `/loop 10m /fld-watch`: the board starts it, and the queue shows whether it runs or waits on me.
type Watcher struct {
	// Name is its Rex session's label and its key in the queue.
	Name string `json:"name"`
	// Dir is the checkout it runs in: one nobody works in, since the session reads other people's code.
	Dir    string `json:"dir"`
	Prompt string `json:"prompt"`
}

// Mine, like the other defaults; "watchers" in the config file replaces the list.
var watchers = []Watcher{
	{"fld-pr-watch", filepath.Join(home, "work", "code", "reports-app-watch"), "/loop 10m /fld-watch"},
}

// watcherCommand runs Claude Code on the watcher's prompt, in the manual permission mode whatever
// my settings default to: the session reads text other people wrote, so only what its checkout's
// allowlist names may run unasked. It gets no --mcp-config: it needs none of those servers.
func watcherCommand(w Watcher) []string {
	return []string{which("claude"), "--permission-mode", "manual", w.Prompt}
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
			return s, true
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
	var failed []string
	for _, w := range watchers {
		if s, ok := findWatcher(sessions, w, recorded[w.Name]); ok {
			ids[w.Name] = s.ID
			continue
		}
		id, e := startWatcher(w)
		if e != nil {
			failed = append(failed, e.Error())
			continue
		}
		ids[w.Name] = id
		started = append(started, w.Name)
	}
	if _, e := updateState(func(s *State) { maps.Copy(s.WatcherSessions, ids) }); e != nil {
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
	it := queueItem{Kind: agentItem, Watcher: true, Key: w.Name, Title: w.Prompt, Dir: w.Dir, Group: closed}
	if rs, ok := findWatcher(sessions, w, recorded); ok {
		it.Agent, it.Session = rs.State, rs.ID
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
