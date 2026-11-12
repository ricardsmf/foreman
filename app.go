package main

// The program: the board and the queue in one pane, side by side or one of them alone. Each keeps
// its own model and keys; the app gives the keys to the side in focus, everything else to both,
// and draws them next to each other.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type side int

const (
	boardSide side = iota
	queueSide
)

const (
	// boardShare is the board's share of the width beside the queue, in tenths.
	boardShare = 6
	// bothMinWidth is the narrowest pane that fits the two side by side; under it, only the side
	// in focus shows.
	bothMinWidth = 110
)

type app struct {
	board         *model
	queue         *queueModel
	focus         side
	zoomed        bool // the side in focus fills the pane
	width, height int
}

func newApp(board *model, queue *queueModel, queueOnly bool) *app {
	a := &app{board: board, queue: queue}
	if queueOnly {
		a.focus, a.zoomed = queueSide, true
	}
	// One load serves both: the board's PRs are the queue's.
	queue.reloadPrs = func() tea.Cmd { return board.reload(prs) }
	return a
}

func (a *app) Init() tea.Cmd { return tea.Batch(a.board.Init(), a.queue.Init()) }

// to hands msg to one side and returns what it asks for.
func (a *app) to(s side, msg tea.Msg) tea.Cmd {
	if s == queueSide {
		_, cmd := a.queue.Update(msg)
		return cmd
	}
	_, cmd := a.board.Update(msg)
	return cmd
}

// alone is whether only the side in focus shows.
func (a *app) alone() bool { return a.zoomed || a.width < bothMinWidth }

// widths are the columns each side gets. A side that isn't showing keeps the whole pane's, so it
// is the right size when it shows.
func (a *app) widths() (board, queue int) {
	if a.alone() {
		return a.width, a.width
	}
	board = (a.width - 1) * boardShare / 10
	return board, a.width - 1 - board
}

// resize tells each side its size.
func (a *app) resize() tea.Cmd {
	if a.width == 0 {
		return nil
	}
	bw, qw := a.widths()
	return tea.Batch(
		a.to(boardSide, tea.WindowSizeMsg{Width: bw, Height: a.height}),
		a.to(queueSide, tea.WindowSizeMsg{Width: qw, Height: a.height}),
	)
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		return a, a.resize()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+l":
			a.focus = 1 - a.focus
			return a, a.resize()
		case "ctrl+z":
			a.zoomed = !a.zoomed
			return a, a.resize()
		}
		return a, a.to(a.focus, msg)
	case tea.PasteMsg:
		return a, a.to(a.focus, msg)
	case openFormMsg:
		a.focus = boardSide
		return a, tea.Batch(a.resize(), a.board.requestForm(msg.key))
	case loadedMsg:
		cmd := a.to(boardSide, msg)
		if msg.which != prs {
			return a, cmd
		}
		list := make([]Pr, 0, len(msg.rows))
		for _, r := range msg.rows {
			if r.Pr != nil {
				list = append(list, *r.Pr)
			}
		}
		return a, tea.Batch(cmd, a.to(queueSide, queuePrsMsg{list, msg.err}))
	}
	// A message one side doesn't know, it ignores.
	return a, tea.Batch(a.to(boardSide, msg), a.to(queueSide, msg))
}

// dim is a line in faint text, for the side that isn't in focus. Bold's end ends faint too, so it
// is put back after each.
func dim(line string) string {
	return "\x1b[2m" + strings.ReplaceAll(line, "\x1b[22m", "\x1b[22;2m") + "\x1b[22m"
}

func (a *app) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.ReportFocus = true
	v.WindowTitle = "foreman"
	if n := a.queue.needYou(); n > 0 {
		v.WindowTitle = fmt.Sprintf("foreman (%d)", n)
	}
	if a.width == 0 {
		return v
	}
	v.SetContent(a.content())
	return v
}

// content is the side in focus alone, or the board, a rule and the queue, the other side faint.
func (a *app) content() string {
	if a.alone() {
		if a.focus == queueSide {
			return a.queue.content()
		}
		return a.board.content()
	}
	bw, qw := a.widths()
	left, right := strings.Split(a.board.content(), "\n"), strings.Split(a.queue.content(), "\n")
	rule := fg(cSurface, "│")
	lines := make([]string, a.height)
	for n := range lines {
		l, r := "", ""
		if n < len(left) {
			l = left[n]
		}
		if n < len(right) {
			r = right[n]
		}
		l, r = fit(l, bw), fit(r, qw)
		if a.focus == queueSide {
			l = dim(l)
		} else {
			r = dim(r)
		}
		lines[n] = l + rule + r
	}
	return strings.Join(lines, "\n")
}
