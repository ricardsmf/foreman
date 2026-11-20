package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// A Dispatched is everything the state keeps of one dispatch, so forgetting its key forgets it all.
type Dispatched struct {
	// Dir is the directory its agent works in.
	Dir string `json:"dir"`
	// Session is its agent's latest Rex session: its ID, which survives a rename, or for dispatches
	// before IDs were kept, its label.
	Session string `json:"session,omitempty"`
	// Agent is the agent dispatched, to resume it with once its session is closed.
	Agent string `json:"agent,omitempty"`
	// Title is the ticket's or PR's summary, for the queue.
	Title string `json:"title,omitempty"`
	// StartedFrom is the commit a ticket's, task's or fix's worktree was on at dispatch: the board
	// diffs the agent's work from there, leaving out a reused branch's earlier commits.
	StartedFrom string `json:"startedFrom,omitempty"`
	// Hidden is whether I dropped its closed session from the queue. The worktree stays dispatched,
	// for the board's cleanup; reopening the session or dispatching again shows it.
	Hidden bool `json:"hidden,omitempty"`
}

type State struct {
	RepoByProject map[string]string `json:"repoByProject"`
	// AgentByRepo maps a repo's main checkout to the agent last dispatched there.
	AgentByRepo map[string]string `json:"agentByRepo,omitempty"`
	// Dispatched maps a ticket, PR or task key to its dispatch.
	Dispatched map[string]Dispatched `json:"dispatches"`
	// Panes maps a Rex session's ID to the pane its agent runs in, as Rex made it at dispatch: the
	// agent's state is read from that pane and replies are typed there, whatever else the session
	// holds. A session without one (older dispatches) gets its oldest pane that reports a status.
	Panes map[string]string `json:"panes,omitempty"`
	// Dismissed maps a queue item to what it showed when marked reviewed: an agent item, by its key,
	// moves up again when its report is newer; a PR item, by "pr:" and its key, when its status changes.
	Dismissed map[string]string `json:"dismissed,omitempty"`
	// WatcherSessions maps each watcher's name to its Rex session ID: it may rename the session.
	WatcherSessions map[string]string `json:"watcherSessions,omitempty"`
	// WatcherPrs maps each polled watcher's name to its PRs' signatures at its last wake, by
	// `owner/name#n`: foreman watch wakes it again only when one differs.
	WatcherPrs map[string]map[string]string `json:"watcherPrs,omitempty"`
	// WatcherStopped holds the watchers I stopped from the queue: nothing starts one again until I
	// start it there, or run it by hand in its checkout.
	WatcherStopped map[string]bool `json:"watcherStopped,omitempty"`
	// WatcherRan maps a scheduled watcher's name to the latest of its times foreman watch has dealt
	// with, as RFC 3339: it wakes the watcher again only when a later one has passed.
	WatcherRan map[string]string `json:"watcherRan,omitempty"`
	// Orchestrator is the orchestrator's Rex session ID: it may rename the session.
	Orchestrator string `json:"orchestrator,omitempty"`
	// Questions holds the question each agent waits on in foreman ask, by its key.
	Questions map[string]Question `json:"questions,omitempty"`
	// Decisions holds the questions each agent went on without, taking its own recommendation,
	// oldest first, until I confirm them or answer otherwise.
	Decisions map[string][]Question `json:"decisions,omitempty"`
	// TaskSeq holds the last task number used in each repo, by the repo's directory name.
	TaskSeq map[string]int `json:"taskSeq,omitempty"`
	// Branches maps a ticket to the branch its worktree was on when cleanup removed it: of the
	// several a ticket may have, its next dispatch goes back to that one, and drops the note.
	Branches map[string]string `json:"branches,omitempty"`
	// Board is the view and the filter the board was last on, which it opens on.
	Board *BoardView `json:"board,omitempty"`
}

// change applies fn to key's dispatch, when it has one.
func (s *State) change(key string, fn func(*Dispatched)) {
	if d, ok := s.Dispatched[key]; ok {
		fn(&d)
		s.Dispatched[key] = d
	}
}

// legacyDispatches is how a state file held the dispatches before each was one record: a map per
// field, by key.
type legacyDispatches struct {
	Dispatched  map[string]string `json:"dispatched"`
	Sessions    map[string]string `json:"sessions"`
	Agents      map[string]string `json:"agents"`
	Titles      map[string]string `json:"titles"`
	StartedFrom map[string]string `json:"startedFrom"`
	Hidden      map[string]bool   `json:"hidden"`
}

func (l legacyDispatches) records() map[string]Dispatched {
	out := map[string]Dispatched{}
	for key, dir := range l.Dispatched {
		out[key] = Dispatched{Dir: dir, Session: l.Sessions[key], Agent: l.Agents[key], Title: l.Titles[key],
			StartedFrom: l.StartedFrom[key], Hidden: l.Hidden[key]}
	}
	return out
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
func lockState() (unlock func(), err error) { return lockFile(stateFile + ".lock") }

// lockFile holds an exclusive lock on file, across processes and within this one, until unlock.
func lockFile(file string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return func() {}, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o644)
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
		// A file from before the records has none: the next save writes them in place of its maps.
		if s.Dispatched == nil {
			var old legacyDispatches
			if json.Unmarshal(b, &old) == nil {
				s.Dispatched = old.records()
			}
		}
	}
	if s.RepoByProject == nil {
		s.RepoByProject = map[string]string{}
	}
	if s.AgentByRepo == nil {
		s.AgentByRepo = map[string]string{}
	}
	if s.Dispatched == nil {
		s.Dispatched = map[string]Dispatched{}
	}
	if s.Panes == nil {
		s.Panes = map[string]string{}
	}
	if s.Dismissed == nil {
		s.Dismissed = map[string]string{}
	}
	if s.WatcherSessions == nil {
		s.WatcherSessions = map[string]string{}
	}
	if s.WatcherPrs == nil {
		s.WatcherPrs = map[string]map[string]string{}
	}
	if s.WatcherStopped == nil {
		s.WatcherStopped = map[string]bool{}
	}
	if s.WatcherRan == nil {
		s.WatcherRan = map[string]string{}
	}
	if s.Questions == nil {
		s.Questions = map[string]Question{}
	}
	if s.Decisions == nil {
		s.Decisions = map[string][]Question{}
	}
	if s.TaskSeq == nil {
		s.TaskSeq = map[string]int{}
	}
	if s.Branches == nil {
		s.Branches = map[string]string{}
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
