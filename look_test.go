package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A row's title leaves out the tags its row has no use for, and what is left starts with a capital.
func TestDisplayTitle(t *testing.T) {
	for in, want := range map[string]string{
		"[Reports App][Tests] the parts save is accepted":       "The parts save is accepted",
		"fix: [FLD-1885] The coordinator PATCH trusts the type": "The coordinator PATCH trusts the type",
		"feat(queue)!: cards for what waits":                    "Cards for what waits",
		"Keep a log of every dispatch":                          "Keep a log of every dispatch",
		"a [bracket] inside stays":                              "a [bracket] inside stays",
		"[Only a tag]":                                          "[Only a tag]",
	} {
		if got := displayTitle(in); got != want {
			t.Errorf("displayTitle(%q) = %q; want %q", in, got, want)
		}
	}
	if got := titleTags("fix: [Reports App][Tests] x"); !slices.Equal(got, []string{"Reports App", "Tests"}) {
		t.Errorf("titleTags = %q", got)
	}
}

var sampleReport = strings.Join([]string{
	"# The log outlives cleanup, in **events.jsonl**.",
	"",
	"## What changed",
	"",
	"- `events.go`: the log.",
	"- Dispatch ids.",
	"",
	"## Checks",
	"",
	"go test passes.",
	"",
	"## Open points",
	"",
	"- Not installed: it needs a build",
	"  and a restart.",
	"- First poll logs old reports.",
}, "\n")

// A report reads as its headline, what it leaves open and the sections the brief leaves out.
func TestParseDoc(t *testing.T) {
	d := parseDoc(strings.Split(sampleReport, "\n"))
	if d.head != "The log outlives cleanup, in events.jsonl." {
		t.Errorf("head %q", d.head)
	}
	name, points := d.open()
	if name != "Open points" || !slices.Equal(points, []string{"Not installed: it needs a build and a restart.", "First poll logs old reports."}) {
		t.Errorf("open: %q %q", name, points)
	}
	var outline []string
	for _, s := range d.outline() {
		outline = append(outline, s.name+" "+strings.Repeat("x", len(s.lines)))
	}
	if !slices.Equal(outline, []string{"What changed xx", "Checks x"}) {
		t.Errorf("outline: %q", outline)
	}
	// A PR's description has no headline: its first sentence stands for one.
	if d := parseDoc([]string{"## Summary", "", "Until now the check ran", "only on Odoo.", "", "## Test plan"}); d.head != "Until now the check ran only on Odoo." {
		t.Errorf("a PR's head: %q", d.head)
	}
	if !openRe.MatchString("Left open") || !openRe.MatchString("Open") || openRe.MatchString("Open comments (2, both valid)") {
		t.Error("openRe")
	}
}

// What waits on me is a card with what it asks or found; what moves without me is a row.
func TestQueueSizeFollowsUrgency(t *testing.T) {
	tempState(t)
	t.Setenv("REX_SESSION", "")
	dispatched(t, "FLD-1", "/w/1")
	dispatched(t, "FLD-2", "/w/2")
	if err := os.WriteFile(filepath.Join(evidenceDir("FLD-2"), "report.md"), []byte(sampleReport), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = updateState(func(s *State) {
		s.Dispatched["FLD-3"] = Dispatched{Dir: "/w/3", Title: "[Reports App] Three", Session: "s3"}
		s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" })
		s.Questions["FLD-1"] = Question{Text: "Which store?", At: time.Now(), Options: []string{"Redis", "Postgres"}, Recommended: "Postgres"}
	})
	q := &queueModel{details: map[string][]string{}, width: 100, height: 40, state: loadState(),
		sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "working"}, {ID: "s3", Label: "fld-3", State: "working", Msg: "Running the tests"}}}
	q.rebuild()
	lines := q.render(100, 40)
	screen := ansi.Strip(strings.Join(lines, "\n"))
	for _, want := range []string{"Your move", "FLD-1 asks you", "Which store?", "1  Redis", "2  Postgres  recommended",
		"FLD-2 done", "The log outlives cleanup", "2 left open", "Agents at work", "Three", "FLD-3 · Running the tests"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the queue lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "Nothing waits on you") || strings.Contains(screen, "[Reports App]") {
		t.Errorf("the queue:\n%s", screen)
	}
	// The cards sit on a surface of their own; the agent at work doesn't.
	for _, l := range lines {
		plain := ansi.Strip(l)
		if strings.Contains(plain, "Which store?") || strings.Contains(plain, "The log outlives") {
			if !strings.Contains(l, bgPanel) && !strings.Contains(l, bgSelected) {
				t.Errorf("a card's line has no surface: %q", l)
			}
		}
		if strings.Contains(plain, "Running the tests") && strings.Contains(l, bgPanel) {
			t.Errorf("the working row is a card: %q", l)
		}
	}

	// space opens the brief, space again the whole text, esc closes it.
	q.input(tea.KeyPressMsg{Code: tea.KeyDown})
	q.input(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	brief := ansi.Strip(strings.Join(q.reader(90, 40), "\n"))
	for _, want := range []string{"FLD-2", "The log outlives cleanup", "Open points", "· Not installed: it needs a build and a restart.", "Also in the report", "What changed", "2 lines", "Checks"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief)
		}
	}
	q.input(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if q.reading != readAll || !strings.Contains(q.keys(), "^u ^d scroll") {
		t.Errorf("space twice: reading %d, keys %q", q.reading, q.keys())
	}
	q.input(tea.KeyPressMsg{Code: tea.KeyEscape})
	if q.reading != 0 || q.reader(90, 40) != nil {
		t.Errorf("esc: reading %d", q.reading)
	}

	// With nothing that waits on me, the queue says so.
	q.state, _ = updateState(func(s *State) {
		delete(s.Questions, "FLD-1")
		delete(s.Dispatched, "FLD-2")
	})
	q.rebuild()
	if screen := ansi.Strip(strings.Join(q.render(100, 40), "\n")); !strings.Contains(screen, "Nothing waits on you.") {
		t.Errorf("nothing pending:\n%s", screen)
	}
}

// The board names each status once, over its rows, and → reads the row under the cursor.
func TestBoardGroupsByStatus(t *testing.T) {
	tempState(t)
	m := &model{inFlight: map[viewKind]bool{}, title: "FLD open tickets", state: loadState(), agents: map[string]string{},
		details: map[string][]string{"FLD-1": {"# FLD-1: One", "", "url", "Type: Bug", "", "The first ticket's description."}},
		issues: []Row{
			{Key: "FLD-1", Summary: "[Reports App] One", Status: "In Progress", Issue: &Issue{}},
			{Key: "FLD-2", Summary: "Two", Status: "In Progress", Issue: &Issue{}},
			{Key: "FLD-3", Summary: "Three", Status: "Blocked", Category: "Blocked", Issue: &Issue{}},
		}}
	board := ansi.Strip(strings.Join(m.renderBoard(80, 20), "\n"))
	if strings.Count(board, "in progress") != 1 || strings.Count(board, "blocked") != 1 || strings.Contains(board, "[Reports App]") {
		t.Errorf("the board:\n%s", board)
	}
	if !strings.Contains(board, "The first ticket's description.") {
		t.Errorf("the row under the cursor shows no peek:\n%s", board)
	}
	m.boardInput(tea.KeyPressMsg{Code: tea.KeyRight})
	if brief := ansi.Strip(strings.Join(m.reader(80, 20), "\n")); m.reading != readBrief || !strings.Contains(brief, "area Reports App") || !strings.Contains(brief, "The first ticket's description.") {
		t.Errorf("→: reading %d\n%s", m.reading, brief)
	}
	if board := ansi.Strip(strings.Join(m.renderBoard(80, 20), "\n")); strings.Contains(board, "The first ticket's description.") {
		t.Errorf("the peek stays while the reading pane shows the row:\n%s", board)
	}
	m.boardInput(tea.KeyPressMsg{Code: tea.KeyRight})
	m.boardInput(tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.reading != readBrief {
		t.Errorf("← from the whole text: reading %d", m.reading)
	}
	m.boardInput(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.reading != 0 {
		t.Errorf("esc: reading %d", m.reading)
	}
}
