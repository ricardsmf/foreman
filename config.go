package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Settings that depend on whose Jira and whose branches these are. The defaults are mine;
// ~/.config/foreman/config.json overrides any of them.
var (
	configFile     = filepath.Join(home, ".config", "foreman", "config.json")
	site           = "https://bloqit.atlassian.net"
	cloudID        = "1eb9eb54-74a9-4d94-8eb1-a1a7696be552" // bloqit.atlassian.net
	defaultProject = "FLD"
	branchPrefix   = "rf/"
)

type config struct {
	Site           *string `json:"site"`
	CloudID        *string `json:"cloudId"`
	DefaultProject *string `json:"defaultProject"`
	BranchPrefix   *string `json:"branchPrefix"`
	// Watchers replaces the default list; an empty one turns them off.
	Watchers *[]Watcher `json:"watchers"`
}

// loadConfig applies the config file, if there is one, over the defaults.
func loadConfig() error {
	b, err := os.ReadFile(configFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var c config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	// A misspelt key would otherwise be ignored without a word.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return fmt.Errorf("%s: %w", tilde(configFile), err)
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&site, c.Site)
	set(&cloudID, c.CloudID)
	set(&defaultProject, c.DefaultProject)
	set(&branchPrefix, c.BranchPrefix)
	if c.Watchers != nil {
		watchers = nil
		for _, w := range *c.Watchers {
			if w.Name == "" || w.Dir == "" || w.Prompt == "" {
				return fmt.Errorf("%s: a watcher needs a name, a dir and a prompt", tilde(configFile))
			}
			if w.At != "" {
				if _, err := parseAt(w.At); err != nil {
					return fmt.Errorf("%s: watcher %s: at %w", tilde(configFile), w.Name, err)
				}
				if len(w.PRs) > 0 {
					return fmt.Errorf("%s: watcher %s has at and prs: the board wakes a watcher by the clock or for its PRs, so make them two watchers", tilde(configFile), w.Name)
				}
			}
			// The board types the prompt on every wake: a loop would start another schedule each time.
			if (len(w.PRs) > 0 || w.At != "") && strings.HasPrefix(strings.TrimSpace(w.Prompt), "/loop ") {
				return fmt.Errorf("%s: the board wakes watcher %s (it has prs or at), so its prompt runs once per wake and can't be a /loop", tilde(configFile), w.Name)
			}
			if w.Marker != "" && len(w.PRs) == 0 {
				return fmt.Errorf("%s: watcher %s has a marker and no prs: the marker says which of its prs are due a review", tilde(configFile), w.Name)
			}
			if (len(w.On) > 0 || w.Batch != 0) && (len(w.PRs) == 0 || w.Marker != "") {
				return fmt.Errorf("%s: watcher %s: on and batch are for a watcher with prs and no marker: a marked one is woken for the PRs due a review", tilde(configFile), w.Name)
			}
			for _, part := range w.On {
				if !slices.Contains(wakeParts, part) {
					return fmt.Errorf("%s: watcher %s: on has %q, which isn't one of %s", tilde(configFile), w.Name, part, strings.Join(wakeParts, ", "))
				}
			}
			if w.Batch < 0 {
				return fmt.Errorf("%s: watcher %s: batch is how many PRs a pass takes, so not %d", tilde(configFile), w.Name, w.Batch)
			}
			if rest, ok := strings.CutPrefix(w.Dir, "~/"); ok {
				w.Dir = filepath.Join(home, rest)
			}
			watchers = append(watchers, w)
		}
	}
	site = strings.TrimRight(site, "/")
	return nil
}
