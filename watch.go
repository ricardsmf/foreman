package main

import (
	"fmt"
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

// startWatcher starts the watcher in a background Rex session named after it.
func startWatcher(w Watcher) error {
	if !exists(w.Dir) {
		return fmt.Errorf("%s: its checkout %s doesn't exist", w.Name, tilde(w.Dir))
	}
	args := append([]string{"new", w.Name, "--cwd", w.Dir, "--window", "Claude", "--"}, watcherCommand(w)...)
	if _, err := sh("", "rex", args...); err != nil {
		return fmt.Errorf("%s: %w", w.Name, err)
	}
	return nil
}

// startWatchers starts each watcher that has no open session, and returns what it did, one line
// each. A session I started by hand under the watcher's name counts as it.
func startWatchers() (started []string, err error) {
	sessions, err := agentSessions()
	if err != nil {
		return nil, err
	}
	var failed []string
	for _, w := range watchers {
		if _, ok := findSession(sessions, w.Name, ""); ok {
			continue
		}
		if e := startWatcher(w); e != nil {
			failed = append(failed, e.Error())
			continue
		}
		started = append(started, w.Name)
	}
	if len(failed) > 0 {
		err = fmt.Errorf("watcher %s", strings.Join(failed, "; "))
	}
	return started, err
}

// watcherItem is the watcher's line in the queue: with the agents that need me when it waits on a
// permission prompt or failed, else by whether it works, waits for its next pass or isn't running.
func watcherItem(w Watcher, sessions []rexSession) queueItem {
	it := queueItem{Kind: agentItem, Watcher: true, Key: w.Name, Title: w.Prompt, Dir: w.Dir, Group: closed}
	if rs, ok := findSession(sessions, w.Name, ""); ok {
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
