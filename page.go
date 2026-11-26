package main

// foreman page: the PR or ticket of the work in a pane, opened beside that pane. Rex's palette
// runs it from inside an agent's session (rex/foreman.lua), where the board is a trip away.

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// prURL asks GitHub for the PR of the branch checked out in dir, or with n, for PR n of dir's
// repo: a review's worktree is detached at its PR's head. Tests put a fake here.
var prURL = func(dir string, n int) (string, error) {
	args := []string{"pr", "view"}
	if n > 0 {
		args = append(args, strconv.Itoa(n))
	}
	out, err := sh(dir, "gh", append(args, "--json", "url", "--jq", ".url")...)
	return strings.TrimSpace(out), err
}

// pageOf is the page of the dispatch key working in dir: its PR, else its ticket. With no
// dispatch, the PR of dir's branch.
func pageOf(key, dir string) (string, error) {
	n := 0
	if _, num, ok := strings.Cut(key, "#"); ok {
		n, _ = strconv.Atoi(num)
	}
	url, err := prURL(dir, n)
	switch {
	case err == nil && url != "":
		return url, nil
	case key == "":
		why := "gh named none"
		if err != nil {
			why = firstLine(err.Error())
		}
		return "", fmt.Errorf("no dispatch runs here, and the branch in %s has no PR: %s", tilde(dir), why)
	case isTicket(key):
		return site + "/browse/" + key, nil
	}
	return "", fmt.Errorf("%s has no PR yet, and no ticket", key)
}

// runPage opens the page beside the pane REX_BLOCK names, as the board opens one beside itself.
func runPage(argv []string, out io.Writer) error {
	fs := newFlags("page")
	in := fs.String("in", "", "")
	if _, err := parseFlags(fs, argv, 0); err != nil {
		return err
	}
	dir := *in
	if dir == "" {
		dir, _ = os.Getwd()
	}
	s := loadState()
	// No dispatch here leaves the PR of dir's branch.
	key, _ := askerKey(s, os.Getenv("REX_BLOCK"), dir)
	// The dispatch's own directory: the pane's program may have moved out of the worktree.
	url, err := pageOf(key, cmp.Or(s.Dispatched[key].Dir, dir))
	if err != nil {
		return err
	}
	if err := browse(url); err != nil {
		return fmt.Errorf("opening %s: %w", url, err)
	}
	fmt.Fprintln(out, "✓ Opened "+url)
	return nil
}
