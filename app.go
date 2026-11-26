package main

// The program: the board and the queue in one pane, side by side or one of them alone. Each keeps
// its own model and keys; the app gives the keys to the side in focus, everything else to both,
// and draws them next to each other, with what the side in focus has open to read over the other.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

type side int

const (
	boardSide side = iota
	queueSide
)

const (
	// boardShare is the board's share of the width beside the queue, in percent.
	boardShare = 45
	// gapWidth is the space between the two.
	gapWidth = 3
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

// Init also has the terminal say when the system switches between light and dark (run resets it).
func (a *app) Init() tea.Cmd {
	return tea.Batch(a.board.Init(), a.queue.Init(), tea.Raw(ansi.SetModeLightDark))
}

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
	board = (a.width - gapWidth) * boardShare / 100
	return board, a.width - gapWidth - board
}

// resize tells each side its size.
func (a *app) resize() tea.Cmd {
	if a.width == 0 {
		return nil
	}
	a.board.unfocused, a.queue.unfocused = a.focus != boardSide, a.focus != queueSide
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
	case uv.LightColorSchemeEvent, uv.DarkColorSchemeEvent:
		// The terminal's colours changed with the system's: both sides take its new answers.
		return a, requestTheme()
	case tea.FocusMsg:
		// The system may have switched between light and dark while the pane was away, and a
		// terminal that doesn't report the switch still answers for its colours now.
		return a, tea.Batch(requestTheme(), a.to(boardSide, msg), a.to(queueSide, msg))
	case closeQueueMsg:
		a.focus, a.zoomed = boardSide, true
		return a, a.resize()
	case openFormMsg:
		a.focus = boardSide
		if msg.pr != nil {
			a.board.requestFix(*msg.pr, msg.context)
			return a, a.resize()
		}
		return a, tea.Batch(a.resize(), a.board.requestForm(msg.key))
	case loadedMsg:
		cmd := a.to(boardSide, msg)
		if msg.which != prs {
			if msg.err != nil {
				return a, cmd
			}
			// The queue's today view lists the tickets no agent is on yet.
			issues := make([]Issue, 0, len(msg.rows))
			for _, r := range msg.rows {
				if r.Issue != nil {
					issues = append(issues, *r.Issue)
				}
			}
			return a, tea.Batch(cmd, a.to(queueSide, queueTicketsMsg{issues}))
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

// content is the side in focus alone, or the board beside the queue, the side out of focus faint
// unless what the other has open to read takes its place. The footer is the focused side's.
func (a *app) content() string {
	if a.alone() {
		if a.focus == queueSide {
			return a.queue.content()
		}
		return a.board.content()
	}
	bw, qw := a.widths()
	var focus sidePane = a.board
	if a.focus == queueSide {
		focus = a.queue
	}
	footer := hints(focus.keys(), a.width-2)
	rows := max(0, a.height-len(footer)-1)
	var left, right []string
	if a.focus == boardSide {
		left = a.board.body(bw, rows)
		if right = a.board.reader(qw, rows); right == nil {
			right = dimAll(a.queue.body(qw, rows))
		}
	} else {
		right = a.queue.body(qw, rows)
		if left = a.queue.reader(bw, rows); left == nil {
			left = dimAll(a.board.body(bw, rows))
		}
	}
	text, c := a.status()
	return frame(beside(left, bw, spaces(gapWidth), right, qw, rows), footer, text, c, a.width, rows)
}

// status is the newer of the two sides' status lines.
func (a *app) status() (string, Color) {
	bt, bc, bat := a.board.statusLine()
	qt, qc, qat := a.queue.statusLine()
	if qt != "" && (bt == "" || qat.After(bat)) {
		return qt, qc
	}
	return bt, bc
}
