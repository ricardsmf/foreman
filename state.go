package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type State struct {
	RepoByProject map[string]string `json:"repoByProject"`
	// AgentByRepo maps a repo's main checkout to the agent last dispatched there.
	AgentByRepo map[string]string `json:"agentByRepo,omitempty"`
	// Dispatched maps a ticket or PR key to the directory its agent works in.
	Dispatched map[string]string `json:"dispatched"`
	// Sessions maps a ticket or PR key to its agent's latest Rex session: its ID, which survives a
	// rename, or for dispatches before IDs were kept, its label.
	Sessions map[string]string `json:"sessions,omitempty"`
	// Panes maps a Rex session's ID to the pane its agent runs in, as Rex made it at dispatch: the
	// agent's state is read from that pane and replies are typed there, whatever else the session
	// holds. A session without one (older dispatches) gets its oldest pane that reports a status.
	Panes map[string]string `json:"panes,omitempty"`
	// Agents maps a ticket or PR key to the agent dispatched on it, to resume it with once its
	// session is closed.
	Agents map[string]string `json:"agents,omitempty"`
	// Titles maps a ticket or PR key to its summary, for the queue.
	Titles map[string]string `json:"titles,omitempty"`
	// StartedFrom maps a ticket, task or fix key to the commit its worktree was on at dispatch: the
	// board diffs the agent's work from there, leaving out a reused branch's earlier commits.
	StartedFrom map[string]string `json:"startedFrom,omitempty"`
	// Dismissed maps a queue item to what it showed when marked reviewed: an agent item, by its key,
	// moves up again when its report is newer; a PR item, by "pr:" and its key, when its status changes.
	Dismissed map[string]string `json:"dismissed,omitempty"`
	// Hidden holds the dispatched keys whose closed session I dropped from the queue. The worktree
	// stays dispatched, for the board's cleanup; reopening the session or dispatching again shows it.
	Hidden map[string]bool `json:"hidden,omitempty"`
	// WatcherSessions maps each watcher's name to its Rex session ID: it may rename the session.
	WatcherSessions map[string]string `json:"watcherSessions,omitempty"`
	// WatcherPrs maps each polled watcher's name to its PRs' signatures at its last wake, by
	// `owner/name#n`: the board wakes it again only when one differs.
	WatcherPrs map[string]map[string]string `json:"watcherPrs,omitempty"`
	// Orchestrator is the orchestrator's Rex session ID: it may rename the session.
	Orchestrator string `json:"orchestrator,omitempty"`
	// Questions holds the question each agent waits on in foreman ask, by its key.
	Questions map[string]Question `json:"questions,omitempty"`
	// TaskSeq holds the last task number used in each repo, by the repo's directory name.
	TaskSeq map[string]int `json:"taskSeq,omitempty"`
	// Board is the view and the filter the board was last on, which it opens on.
	Board *BoardView `json:"board,omitempty"`
}

type BoardView struct {
	Prs      bool   `json:"prs,omitempty"`      // the PR view rather than the tickets
	Mine     bool   `json:"mine,omitempty"`     // the tickets filtered to mine
	PrFilter string `json:"prFilter,omitempty"` // "all", "requested" or "mine"
}

// stateMu serialises read-modify-write cycles: the board and dispatch goroutines both update the file.
var stateMu sync.Mutex

// lockState does the same across processes: the board and the queue each rewrite the file, and
// without the lock one's write drops what the other wrote since its read.
func lockState() (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		return func() {}, err
	}
	f, err := os.OpenFile(stateFile+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return func() {}, err
	}
	// Closing the file releases the lock.
	return func() { _ = f.Close() }, nil
}

func loadState() State {
	stateMu.Lock()
	defer stateMu.Unlock()
	return readState()
}

func readState() State {
	var s State
	if b, err := os.ReadFile(stateFile); err == nil {
		if json.Unmarshal(b, &s) != nil {
			// Keep the unreadable file for inspection instead of overwriting it on the next save.
			_ = os.Rename(stateFile, stateFile+".corrupt")
			s = State{}
		}
	}
	if s.RepoByProject == nil {
		s.RepoByProject = map[string]string{}
	}
	if s.AgentByRepo == nil {
		s.AgentByRepo = map[string]string{}
	}
	if s.Dispatched == nil {
		s.Dispatched = map[string]string{}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]string{}
	}
	if s.Panes == nil {
		s.Panes = map[string]string{}
	}
	if s.Agents == nil {
		s.Agents = map[string]string{}
	}
	if s.Titles == nil {
		s.Titles = map[string]string{}
	}
	if s.StartedFrom == nil {
		s.StartedFrom = map[string]string{}
	}
	if s.Dismissed == nil {
		s.Dismissed = map[string]string{}
	}
	if s.Hidden == nil {
		s.Hidden = map[string]bool{}
	}
	if s.WatcherSessions == nil {
		s.WatcherSessions = map[string]string{}
	}
	if s.WatcherPrs == nil {
		s.WatcherPrs = map[string]map[string]string{}
	}
	if s.Questions == nil {
		s.Questions = map[string]Question{}
	}
	if s.TaskSeq == nil {
		s.TaskSeq = map[string]int{}
	}
	return s
}

// updateState applies change to the state on disk and writes it back atomically.
func updateState(change func(*State)) (State, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	unlock, err := lockState()
	defer unlock()
	s := readState()
	change(&s)
	if err != nil {
		return s, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return s, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(stateFile), ".state-*.json")
	if err != nil {
		return s, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return s, err
	}
	if err := tmp.Close(); err != nil {
		return s, err
	}
	return s, os.Rename(tmp.Name(), stateFile)
}
