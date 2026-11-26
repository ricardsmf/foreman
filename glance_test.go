package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The glance holds what waits on me, and for each agent the PR on its worktree's branch with
// what that PR's row says.
func TestGlanceOfTheQueue(t *testing.T) {
	q, _, dir, _ := conflictQueue(t)
	dispatched(t, "FLD-1", dir)
	dispatched(t, "FLD-2", t.TempDir())
	q.prs = append(q.prs, Pr{Key: "app#9", Summary: "Other", Repo: "me/app", Number: 9, Branch: "rf/other", Base: "develop",
		Checkout: dir, Mine: true, Review: "approved", Status: "approved ✓", Category: "Done"})
	q.state = loadState()
	write := q.rebuildGlance(t)
	if write == nil {
		t.Fatal("a queue that changed wrote no glance")
	}
	write()
	g := readGlance()
	if g == nil {
		t.Fatal("no glance read back, with its foreman running")
	}
	if want := []string{"pr:app#7"}; !slices.Equal(g.Needs, want) {
		t.Errorf("needs %q, want %q", g.Needs, want)
	}
	if want := []string{"FLD-1", "FLD-2", "pr:app#9"}; !slices.Equal(slices.Sorted(slices.Values(g.Review)), want) {
		t.Errorf("review %q, want %q", g.Review, want)
	}
	if want := (glancePr{Key: "app#7", Number: 7, Says: "conflicts with develop", Group: needsYou}); len(g.Prs) != 1 || g.Prs["FLD-1"] != want {
		t.Errorf("prs %+v, want FLD-1 on %+v", g.Prs, want)
	}
	// Nothing changed: nothing is written.
	if again := q.rebuildGlance(t); again != nil {
		t.Error("the same queue wrote its glance again")
	}

	// A glance whose foreman is gone is none.
	old := askRuns
	askRuns = func(int) bool { return false }
	t.Cleanup(func() { askRuns = old })
	if readGlance() != nil {
		t.Error("read the glance of a foreman that no longer runs")
	}
}

// rebuildGlance rebuilds the queue and returns the glance's write alone.
func (q *queueModel) rebuildGlance(t *testing.T) func() {
	t.Helper()
	q.all = queueItems(q.state, q.sessions, q.prs)
	cmd := q.glance()
	if cmd == nil {
		return nil
	}
	return func() { cmd() }
}

func TestStatusLine(t *testing.T) {
	g := &glance{Pid: 1, Needs: []string{"FLD-1", "pr:app#7", "FLD-3"}, Review: []string{"FLD-2", "pr:app#9"},
		Prs: map[string]glancePr{
			"FLD-1": {Key: "app#7", Number: 7, Says: "conflicts with develop", Group: needsYou},
			"FLD-2": {Key: "app#9", Number: 9, Says: "ready to merge", Group: toReview},
			"FLD-4": {Key: "app#4", Number: 4, Says: "with ana", Group: waiting},
		}}
	for _, tc := range []struct {
		name, key string
		g         *glance
		want      string
	}{
		{"its own row and its PR's aren't elsewhere", "FLD-1", g, "FLD-1 · #7 conflicts with develop · elsewhere 1 needs you, 2 to review"},
		{"a PR to merge", "FLD-2", g, "FLD-2 · #9 ready to merge · elsewhere 3 need you"},
		{"a PR that waits on others", "FLD-4", g, "FLD-4 · #4 with ana · elsewhere 3 need you, 2 to review"},
		{"no PR", "task:app-1", g, "task:app-1 · elsewhere 3 need you, 2 to review"},
		{"nothing waits", "FLD-1", &glance{Pid: 1}, "FLD-1"},
		{"no foreman runs", "FLD-1", nil, "FLD-1"},
		{"not a dispatch", "", g, "elsewhere 3 need you, 2 to review"},
		{"neither", "", nil, ""},
	} {
		if got := ansi.Strip(statusLine(tc.key, tc.g)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// Red asks for me and green is to review, as in the queue.
	line := statusLine("FLD-1", g)
	if !strings.Contains(line, fg(cRed, "#7 conflicts with develop")) || !strings.Contains(line, fg(cRed, "1 needs you")) || !strings.Contains(line, fg(cGreen, "2 to review")) {
		t.Errorf("colours: %q", line)
	}
}

// The status line's dispatch is its pane's: two dispatches may share a worktree, a ticket's and
// the fix of its PR.
func TestStatusLineFindsItsDispatch(t *testing.T) {
	useFakeRex(t)
	tempState(t)
	dir, other := filepath.Join(t.TempDir(), "app-fld-1"), filepath.Join(t.TempDir(), "app-fld-5")
	s, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"] = Dispatched{Dir: dir, Session: "s1"}
		s.Dispatched["fix:app#7"] = Dispatched{Dir: dir, Session: "s2"}
		s.Dispatched["FLD-5"] = Dispatched{Dir: other, Session: "s5"}
		s.Panes["s1"], s.Panes["s2"] = "block:1", "block:2"
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ session, block, dir, want string }{
		{"", "block:2", dir, "fix:app#7"},
		{"", "block:1", dir, "FLD-1"},
		{"s2", "block:1", dir, "fix:app#7"},
		{"", "block:9", filepath.Join(other, "sub"), "FLD-5"},
		{"", "", "/elsewhere", ""},
	} {
		if got := paneDispatch(s, tc.session, tc.block, tc.dir); got != tc.want {
			t.Errorf("session %q, pane %q in %s: %q, want %q", tc.session, tc.block, tc.dir, got, tc.want)
		}
	}

	// The command, as Claude Code runs it: the directory it started in is on stdin.
	b, _ := json.Marshal(glance{Pid: os.Getpid(), Needs: []string{"FLD-9"}})
	if err := os.WriteFile(glanceFile(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_SESSION", "")
	t.Setenv("REX_BLOCK", "block:9")
	in := `{"cwd":"/moved/to","workspace":{"current_dir":"/moved/to","project_dir":"` + other + `"}}`
	var out bytes.Buffer
	if err := runStatusline(strings.NewReader(in), &out); err != nil || ansi.Strip(out.String()) != "FLD-5 · elsewhere 1 needs you\n" {
		t.Errorf("printed %q, %v", out.String(), err)
	}
	// Outside a dispatch with nothing waiting, it prints nothing rather than an empty line.
	if err := os.Remove(glanceFile()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runStatusline(strings.NewReader(`{"cwd":"/elsewhere"}`), &out); err != nil || out.Len() != 0 {
		t.Errorf("printed %q, %v", out.String(), err)
	}
}

// A dispatched Claude Code gets foreman's status line over my settings; pi has none to give.
func TestDispatchedClaudeShowsTheStatusLine(t *testing.T) {
	for _, agent := range []string{agentClaude, agentAuto, "Claude (yolo)"} {
		for name, cmd := range map[string][]string{"start": agentArgs(agent), "resume": resumeCommand(agent, "go on")} {
			at := slices.Index(cmd, "--settings")
			if at < 0 {
				t.Fatalf("%s, %s: no --settings in %q", agent, name, cmd)
			}
			var got struct {
				StatusLine struct {
					Type, Command   string
					RefreshInterval int
				}
			}
			if err := json.Unmarshal([]byte(cmd[at+1]), &got); err != nil {
				t.Fatalf("%s, %s: %v in %s", agent, name, err, cmd[at+1])
			}
			if sl := got.StatusLine; sl.Type != "command" || !strings.HasSuffix(sl.Command, "foreman' statusline") || sl.RefreshInterval != statusLineEvery {
				t.Errorf("%s, %s: status line %+v", agent, name, sl)
			}
		}
	}
	if slices.Contains(agentArgs(agentPi), "--settings") {
		t.Error("pi was given Claude Code's settings")
	}
}
