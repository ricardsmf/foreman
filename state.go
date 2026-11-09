package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type State struct {
	RepoByProject map[string]string `json:"repoByProject"`
	Agent         string            `json:"agent,omitempty"`
	// Dispatched maps a ticket or PR key to the directory its agent works in.
	Dispatched map[string]string `json:"dispatched"`
	// Sessions maps a ticket or PR key to its agent's latest Rex session label.
	Sessions map[string]string `json:"sessions,omitempty"`
	// Titles maps a ticket or PR key to its summary, for the queue.
	Titles map[string]string `json:"titles,omitempty"`
	// Dismissed maps a queue item to what it showed when dismissed: an agent item comes back when
	// its report is newer, a PR item when its status changes.
	Dismissed map[string]string `json:"dismissed,omitempty"`
}

// stateMu serialises read-modify-write cycles: the board and dispatch goroutines both update the file.
var stateMu sync.Mutex

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
	if s.Dispatched == nil {
		s.Dispatched = map[string]string{}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]string{}
	}
	if s.Titles == nil {
		s.Titles = map[string]string{}
	}
	if s.Dismissed == nil {
		s.Dismissed = map[string]string{}
	}
	return s
}

// updateState applies change to the state on disk and writes it back atomically.
func updateState(change func(*State)) (State, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	s := readState()
	change(&s)
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
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
