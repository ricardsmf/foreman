package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func testApp(t *testing.T, queueOnly bool) *app {
	t.Helper()
	useFakeRex(t)
	m := &model{
		inFlight: map[viewKind]bool{}, lastLoad: map[viewKind]time.Time{}, details: map[string][]string{}, detailOK: map[string]bool{},
		agents: map[string]string{}, prFilter: "all", statusColor: cOverlay, title: "FLD open tickets",
		issues: []Row{issueRow(Issue{Key: "FLD-7", Summary: "Seven"})},
	}
	q := &queueModel{details: map[string][]string{}, statusColor: cOverlay}
	a := newApp(m, q, queueOnly)
	a.Update(tea.WindowSizeMsg{Width: 161, Height: 30})
	return a
}

func keyOf(code rune, mod tea.KeyMod, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: mod, Text: text}
}

func TestAppLayouts(t *testing.T) {
	a := testApp(t, false)
	if a.board.width != 96 || a.queue.width != 64 || a.board.height != 30 || a.queue.height != 30 {
		t.Fatalf("side by side: board %dx%d, queue %dx%d; want 96 and 64 of 161 wide", a.board.width, a.board.height, a.queue.width, a.queue.height)
	}
	lines := strings.Split(a.content(), "\n")
	if len(lines) != 30 {
		t.Fatalf("%d lines; want the pane's 30", len(lines))
	}
	for n, l := range lines {
		if w := ansi.StringWidth(l); w != 161 {
			t.Fatalf("line %d is %d wide; want 161: %q", n, w, ansi.Strip(l))
		}
	}
	if top := ansi.Strip(lines[0]); !strings.Contains(top, "foreman") || !strings.Contains(top, "│ queue") {
		t.Errorf("the first line lacks the board, the rule or the queue: %q", top)
	}
	// The queue isn't in focus: its half is faint, the board's isn't.
	board, queue, _ := strings.Cut(lines[0], "│")
	if strings.Contains(board, "\x1b[2m") || !strings.Contains(queue, "\x1b[2m") {
		t.Errorf("faint is on the wrong side: %q │ %q", board, queue)
	}

	// ^z: the side in focus alone, at the pane's width.
	a.Update(keyOf('z', tea.ModCtrl, ""))
	if a.board.width != 161 || strings.Contains(ansi.Strip(a.content()), "│ queue") {
		t.Errorf("zoomed on the board: board %d wide, content has the queue", a.board.width)
	}
	// ^l while zoomed: the other side alone.
	a.Update(keyOf('l', tea.ModCtrl, ""))
	if a.focus != queueSide || a.queue.width != 161 || !strings.HasPrefix(ansi.Strip(a.content()), " queue") {
		t.Errorf("zoomed on the queue: focus %d, queue %d wide, content %q", a.focus, a.queue.width, firstLine(ansi.Strip(a.content())))
	}
	a.Update(keyOf('z', tea.ModCtrl, ""))
	if a.alone() || a.queue.width != 64 {
		t.Errorf("^z again: alone %t, queue %d wide; want both", a.alone(), a.queue.width)
	}
	// Too narrow for both: the side in focus alone.
	a.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if !a.alone() || a.queue.width != 90 {
		t.Errorf("a narrow pane: alone %t, queue %d wide", a.alone(), a.queue.width)
	}
}

func TestAppOpensOnTheQueue(t *testing.T) {
	a := testApp(t, true)
	if a.focus != queueSide || !a.alone() || !strings.HasPrefix(ansi.Strip(a.content()), " queue") {
		t.Errorf("foreman queue: focus %d, alone %t, content %q; want the queue alone", a.focus, a.alone(), firstLine(ansi.Strip(a.content())))
	}
}

// Keys and pastes go to the side in focus only; what one side's work reports stays on that side.
func TestAppRoutes(t *testing.T) {
	a := testApp(t, false)
	a.Update(keyOf('c', 0, "c"))
	if a.board.query != "c" || a.queue.showClosed {
		t.Errorf("a key on the board: query %q, queue showClosed %t", a.board.query, a.queue.showClosed)
	}
	a.Update(tea.PasteMsg{Content: "x"})
	if a.board.query != "cx" {
		t.Errorf("a paste on the board: query %q", a.board.query)
	}
	a.Update(keyOf('l', tea.ModCtrl, ""))
	a.Update(keyOf('c', 0, "c"))
	if a.board.query != "cx" || !a.queue.showClosed {
		t.Errorf("a key on the queue: board query %q, queue showClosed %t", a.board.query, a.queue.showClosed)
	}
	a.Update(statusMsg{text: "from the board", color: cRed})
	a.Update(queueStatusMsg{text: "from the queue", color: cRed})
	if a.board.status != "from the board" || a.queue.status != "from the queue" {
		t.Errorf("status lines: board %q, queue %q", a.board.status, a.queue.status)
	}
}

// One PR load serves both sides, and the queue asks the board for it instead of loading its own.
func TestAppSharesPrs(t *testing.T) {
	a := testApp(t, false)
	mine := Pr{Key: "app#5", Summary: "Fix it", Mine: true, Category: "Blocked", Status: "changes"}
	a.board.inFlight[prs] = true
	a.Update(loadedMsg{which: prs, rows: []Row{prRow(mine), prRow(Pr{Key: "app#6", Summary: "Other"})}})
	if len(a.board.prs) != 2 || len(a.queue.prs) != 2 {
		t.Fatalf("after the board's load: board %d PRs, queue %d; want 2 and 2", len(a.board.prs), len(a.queue.prs))
	}
	if n := a.queue.count(needsYou); n != 1 {
		t.Errorf("the queue lists %d PRs that need me; want mine", n)
	}
	if cmd := a.queue.loadPrs(); cmd == nil || !a.board.inFlight[prs] || a.queue.prsLoading {
		t.Errorf("the queue's reload: cmd nil %t, board loading %t, queue loading %t; want the board's load", cmd == nil, a.board.inFlight[prs], a.queue.prsLoading)
	}
	// A failed load is the board's to report; the queue keeps the PRs it had.
	a.Update(loadedMsg{which: prs, err: errors.New("gh: boom")})
	if len(a.queue.prs) != 2 || !strings.Contains(a.board.status, "boom") {
		t.Errorf("after a failed load: queue %d PRs, board status %q", len(a.queue.prs), a.board.status)
	}
	// Tickets are the board's alone.
	a.Update(loadedMsg{which: tickets, rows: []Row{issueRow(Issue{Key: "FLD-9"})}})
	if len(a.queue.prs) != 2 {
		t.Errorf("a ticket load changed the queue's PRs: %d", len(a.queue.prs))
	}
}

// enter on a PR with no agent opens the board's dispatch form on its ticket, and the board takes focus.
func TestAppOpensTheFormFromTheQueue(t *testing.T) {
	a := testApp(t, true)
	cmd := a.queue.requestForm(Pr{Key: "app#5", Summary: "FLD-7 Fix it"})
	if cmd == nil {
		t.Fatal("a PR that names a ticket: want a request")
	}
	a.Update(cmd())
	if a.focus != boardSide || a.board.form == nil || a.board.form.row.Key != "FLD-7" {
		t.Errorf("focus %d, form %+v; want the board's form on FLD-7", a.focus, a.board.form)
	}
	// Zoomed on the queue before, so now the board alone shows the form.
	if !a.alone() || strings.HasPrefix(ansi.Strip(a.content()), " queue") {
		t.Errorf("content after the request: %q", firstLine(ansi.Strip(a.content())))
	}
}

func TestDim(t *testing.T) {
	if got, want := dim("a"+bold("b")+"c"), "\x1b[2ma\x1b[1mb\x1b[22;2mc\x1b[22m"; got != want {
		t.Errorf("dim = %q; want %q", got, want)
	}
}
