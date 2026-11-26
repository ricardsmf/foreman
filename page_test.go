package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakePrs answers prURL from a map by directory and PR number, and notes what was asked.
func fakePrs(t *testing.T, urls map[string]string) *[]string {
	t.Helper()
	var asked []string
	old := prURL
	prURL = func(dir string, n int) (string, error) {
		q := filepath.Base(dir)
		if n > 0 {
			q += "#" + strconv.Itoa(n)
		}
		asked = append(asked, q)
		if u, ok := urls[q]; ok {
			return u, nil
		}
		return "", errors.New("no pull requests found for branch")
	}
	t.Cleanup(func() { prURL = old })
	return &asked
}

// The page of a pane is its dispatch's PR, else its ticket; a review's is the PR it reviews, by
// number, since its worktree is on no branch.
func TestPageOf(t *testing.T) {
	fakePrs(t, map[string]string{"app-fld-1": "https://github.com/o/app/pull/7", "app-pr-5#5": "https://github.com/o/app/pull/5", "tool": "https://github.com/o/tool/pull/2"})
	for _, tc := range []struct{ name, key, dir, want, wantErr string }{
		{"a ticket with a PR", "FLD-1", "/w/app-fld-1", "https://github.com/o/app/pull/7", ""},
		{"a ticket without one", "FLD-2", "/w/app-fld-2", site + "/browse/FLD-2", ""},
		{"a review", "app#5", "/w/app-pr-5", "https://github.com/o/app/pull/5", ""},
		{"a fix", "fix:app#5", "/w/app-pr-5", "https://github.com/o/app/pull/5", ""},
		{"a task without a PR", "task:app-3", "/w/app-task-3", "", "task:app-3 has no PR yet, and no ticket"},
		{"no dispatch, a branch with a PR", "", "/w/tool", "https://github.com/o/tool/pull/2", ""},
		{"no dispatch, no PR", "", "/w/other", "", "no dispatch runs here, and the branch in /w/other has no PR: no pull requests found for branch"},
	} {
		got, err := pageOf(tc.key, tc.dir)
		if got != tc.want || (err == nil) != (tc.wantErr == "") || (err != nil && err.Error() != tc.wantErr) {
			t.Errorf("%s: %q, %v; want %q, %q", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// foreman page opens it beside the pane REX_BLOCK names, whose dispatch it finds by that pane, and
// asks GitHub in the dispatch's worktree wherever the pane's program is.
func TestPageOpensBesideItsPane(t *testing.T) {
	rex := useFakeRex(t)
	tempState(t)
	b := useFakeBrowsers(t, "block:2", `{"browsers": []}`, nil)
	asked := fakePrs(t, map[string]string{"app-fld-1": "https://github.com/o/app/pull/7"})
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"] = Dispatched{Dir: "/w/app-fld-1", Session: "s2"}
		s.Panes["s2"] = "block:2"
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPage([]string{"--in", "/somewhere/else"}, &out); err != nil {
		t.Fatal(err)
	}
	const url = "https://github.com/o/app/pull/7"
	if want := "split block:2 " + strings.Join(browserPaneCmd(url), "\x00"); len(rex.calls) != 1 || rex.calls[0] != want || len(b.opened) != 0 {
		t.Errorf("rex %q, system browser %q; want only %q", rex.calls, b.opened, want)
	}
	if len(*asked) != 1 || (*asked)[0] != "app-fld-1" || out.String() != "✓ Opened "+url+"\n" {
		t.Errorf("asked GitHub in %q, printed %q", *asked, out.String())
	}

	// Nothing to open is an error, which the palette shows.
	t.Setenv("REX_BLOCK", "block:9")
	if err := runPage([]string{"--in", "/w/none"}, &out); err == nil || !strings.Contains(err.Error(), "no dispatch runs here") || len(rex.calls) != 1 {
		t.Errorf("a pane with no page: %v, rex %q", err, rex.calls)
	}
}
