package main

import (
	"flag"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

var updateScreens = flag.Bool("update", false, "rewrite testdata/screens from what the screens draw now")

// looks is a screen as the terminal shows it: each line's text, then the runs of cells that carry
// a style, by column, with their colours and attributes. Two encodings of one look read the same.
func looks(s string) string {
	var b strings.Builder
	for _, line := range uv.NewStyledString(s).Lines(ansi.WcWidth) {
		var text strings.Builder
		var runs []string
		from, last := 0, ""
		end := func(to int) {
			if last != "" {
				runs = append(runs, fmt.Sprintf("%d-%d %s", from, to-1, last))
			}
		}
		for x, c := range line {
			text.WriteString(c.Content)
			if s := styleOf(c.Style); s != last {
				end(x)
				from, last = x, s
			}
		}
		end(len(line))
		b.WriteString(strings.TrimRight(text.String(), " ") + "\n")
		for _, r := range runs {
			b.WriteString("    " + r + "\n")
		}
	}
	return b.String()
}

func styleOf(s uv.Style) string {
	var parts []string
	if s.Fg != nil {
		parts = append(parts, "fg="+colourName(s.Fg))
	}
	if s.Bg != nil {
		parts = append(parts, "bg="+colourName(s.Bg))
	}
	for _, a := range []struct {
		bit  uint8
		name string
	}{{uv.AttrBold, "bold"}, {uv.AttrFaint, "faint"}, {uv.AttrItalic, "italic"}, {uv.AttrReverse, "reverse"}} {
		if s.Attrs&a.bit != 0 {
			parts = append(parts, a.name)
		}
	}
	if s.Underline != 0 {
		parts = append(parts, "underline")
	}
	return strings.Join(parts, " ")
}

func colourName(c color.Color) string {
	switch c := c.(type) {
	case ansi.BasicColor:
		return fmt.Sprintf("ansi%d", c)
	case ansi.IndexedColor:
		return fmt.Sprintf("index%d", c)
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// onBackground is whether some of line sits on bg.
func onBackground(line string, bg color.Color) bool {
	for _, l := range uv.NewStyledString(line).Lines(ansi.WcWidth) {
		for _, c := range l {
			if c.Style.Bg != nil && colourName(c.Style.Bg) == colourName(bg) {
				return true
			}
		}
	}
	return false
}

// matchScreen compares a screen's look with testdata/screens/<name>.txt, or writes it there
// under -update.
func matchScreen(t *testing.T, name, screen string) {
	t.Helper()
	path := filepath.Join("testdata", "screens", name+".txt")
	got := looks(screen)
	if *updateScreens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to record it)", err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for n := range max(len(gl), len(wl)) {
			g, w := "", ""
			if n < len(gl) {
				g = gl[n]
			}
			if n < len(wl) {
				w = wl[n]
			}
			if g != w {
				t.Errorf("%s differs from %s at its line %d:\n got  %q\n want %q", name, path, n+1, g, w)
				return
			}
		}
	}
}

func screenBoard(t *testing.T) *model {
	t.Helper()
	m := &model{
		inFlight: map[viewKind]bool{}, lastLoad: map[viewKind]time.Time{}, details: map[string][]string{}, detailOK: map[string]bool{},
		agents: map[string]string{"s1": "working"}, prFilter: "all", statusColor: cOverlay, title: "FLD open tickets", state: loadState(),
		issues: []Row{
			issueRow(Issue{Key: "FLD-1", Summary: "[Reports App] Removal report PDFs: show the totals", Status: "In Progress", Category: "In Progress", Assignee: "Ricardo Ferreira", Mine: true}),
			issueRow(Issue{Key: "FLD-2", Summary: "Investigate discrepancy in spare parts consumption between DeBloq and P&I", Status: "In Progress", Category: "In Progress", Assignee: "Álvaro Fontaínhas Terroso"}),
			issueRow(Issue{Key: "FLD-3", Summary: "Sync manufacturer SKU to Airtable", Status: "Blocked", Category: "In Progress"}),
			issueRow(Issue{Key: "FLD-4", Summary: "Odoo: one retrofit block per visit", Status: "In Review", Category: "In Progress", Assignee: "ivan.zarro"}),
			issueRow(Issue{Key: "FLD-5", Summary: "Daily Slack retrofit report", Status: "To Do", Category: "To Do", Assignee: "ivan.zarro"}),
		},
	}
	m.details["FLD-2"] = []string{
		"# Investigate discrepancy", "", "There are **significant** discrepancies between the `DeBloq` and P&I numbers.", "",
		"## Context", "", "- Large differences detected", "- Airtable comparison views:", "  - [Less records](https://airtable.com/appP53BGzZZAfQvrf/tbltJb8iqIJK4G18G)", "",
		"```", "select * from consumption", "```",
	}
	m.detailOK["FLD-2"] = true
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

// TestScreensLook pins how the main screens look, cell by cell: what they say, in which colours
// and weights. A change to the styling that means to keep the look keeps these.
func TestScreensLook(t *testing.T) {
	useFakeRex(t, rexSession{ID: "s1", Label: "fld-1", State: "working"})
	tempState(t)
	tempHome(t)
	t.Setenv("REX_BLOCK", "")
	dispatched(t, "FLD-1", "/w/app-fld-1")
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" }) })

	m := screenBoard(t)
	m.cursor = 1
	matchScreen(t, "board", m.content())
	m.reading = readAll
	matchScreen(t, "board-reading", m.content())
	m.reading = 0
	m.help = true
	matchScreen(t, "board-help", m.content())
	m.help = false
	m.openForm(m.issues[1])
	m.form.context = "Look at the Airtable automation\nfirst"
	m.form.contextBack = 6
	m.form.field = fieldContext
	matchScreen(t, "board-form", m.content())

	a := testApp(t, false)
	matchScreen(t, "app", a.content())

	dispatched(t, "FLD-2", "/w/2")
	if err := os.WriteFile(filepath.Join(evidenceDir("FLD-2"), "report.md"), []byte(sampleReport), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = updateState(func(s *State) {
		s.Dispatched["FLD-3"] = Dispatched{Dir: "/w/3", Title: "[Reports App] Three", Session: "s3"}
		s.Questions["FLD-1"] = Question{Text: "Which store?", At: time.Now(), Options: []string{"Redis", "Postgres"}, Recommended: "Postgres"}
	})
	q := &queueModel{details: map[string][]string{}, width: 100, height: 40, state: loadState(), statusColor: cOverlay,
		sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "working"}, {ID: "s3", Label: "fld-3", State: "working", Msg: "Running the tests"}}}
	q.rebuild()
	matchScreen(t, "queue", q.content())
	q.input(tea.KeyPressMsg{Code: tea.KeyDown})
	q.input(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	matchScreen(t, "queue-brief", q.content())
	q.input(tea.KeyPressMsg{Code: tea.KeyEscape})
	q.input(tea.KeyPressMsg{Code: tea.KeyTab})
	matchScreen(t, "queue-today", q.content())

	// The greys a light terminal gets.
	sub, over, sur, sel, pan := cSubtext, cOverlay, cSurface, bgSelected, bgPanel
	t.Cleanup(func() { cSubtext, cOverlay, cSurface, bgSelected, bgPanel = sub, over, sur, sel, pan })
	setTheme(color.Black, color.White)
	m = screenBoard(t)
	m.cursor = 1
	matchScreen(t, "board-light", m.content())
}
