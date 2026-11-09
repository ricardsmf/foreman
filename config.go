package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	site = strings.TrimRight(site, "/")
	return nil
}
