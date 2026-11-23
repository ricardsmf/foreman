package main

// What both sides draw with: lines cut or padded to a width, two columns side by side, a list
// under quiet headings, the reading pane's surface, and titles without their tags.

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func textWidth(s string) int { return ansi.StringWidth(s) }
func spaces(n int) string    { return strings.Repeat(" ", max(0, n)) }

// cut shortens s to width columns, with an ellipsis where it cuts.
func cut(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

// lr puts left and right at the two ends of width columns; left gives way.
func lr(left, right string, width int) string {
	switch rw := textWidth(right); {
	case rw == 0:
		return fit(left, width)
	case rw >= width:
		return fit(right, width)
	default:
		return fit(left, width-rw-2) + "  " + right
	}
}

// paint is line on the background bg across width columns.
func paint(bg, line string, width int) string {
	return "\x1b[" + bg + "m" + fit(line, width) + "\x1b[49m"
}

// tall pads or cuts lines to h rows.
func tall(lines []string, h int) []string {
	out := make([]string, max(0, h))
	copy(out, lines)
	return out
}

// beside sets two columns next to each other, h rows of them.
func beside(left []string, lw int, gap string, right []string, rw, h int) []string {
	left, right = tall(left, h), tall(right, h)
	out := make([]string, len(left))
	for n := range out {
		out[n] = fit(left[n], lw) + gap + fit(right[n], rw)
	}
	return out
}

func indent(lines []string, n int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = spaces(n) + l
	}
	return out
}

func dimAll(lines []string) []string {
	out := make([]string, len(lines))
	for n, l := range lines {
		out[n] = dim(l)
	}
	return out
}

// scrollSpan is a list's first line, moved just far enough to show lines from to to.
func scrollSpan(top, from, to, rows, total int) int {
	if to >= top+rows {
		top = to - rows + 1
	}
	if from < top {
		top = from
	}
	return max(0, min(top, max(0, total-rows)))
}

// A listEntry is a row of a list and the lines it takes, in the group its heading names.
type listEntry struct {
	group string
	tone  Color // the heading's colour
	lines []string
}

// groupedList lays out the entries under a heading wherever their group changes, rows lines of
// them from *top, which it moves to show the cursor's entry and the heading over it.
func groupedList(entries []listEntry, cursor int, top *int, rows int) []string {
	var lines []string
	from, to := 0, 0
	for n, e := range entries {
		start := len(lines)
		if n == 0 || entries[n-1].group != e.group {
			if n > 0 {
				lines = append(lines, "")
			}
			if e.group != "" {
				lines = append(lines, " "+fg(e.tone, e.group))
			}
		}
		lines = append(lines, e.lines...)
		if n == cursor {
			from, to = start, len(lines)-1
		}
	}
	*top = scrollSpan(*top, from, to, rows, len(lines))
	return window(lines, top, rows)
}

// row is a list's line for an entry: the bar that marks the cursor, the text, and what goes at
// the right edge, on the selection's background under the cursor.
func row(sel bool, mark, text, right string, width int) string {
	left := " " + mark + " "
	if sel {
		left = fg(cMauve, "▌") + mark + " "
	}
	line := left + lr(text, right, width-textWidth(left)-1)
	if sel {
		return selected(line, width)
	}
	return line
}

// peek is what the row under the cursor shows under itself: a few lines of what it holds, on
// the selection's background, from column in.
func peek(lines []string, in, width int) []string {
	var out []string
	for _, l := range lines {
		out = append(out, selected(fg(cMauve, "▌")+spaces(in-1)+fg(cSubtext, l), width))
	}
	return out
}

// panel is the reading pane, h lines of text on a surface a shade off the background.
func panel(text []string, width, h int) []string {
	out := []string{paint(bgPanel, "", width)}
	for _, l := range tall(text, h-1) {
		out = append(out, paint(bgPanel, "   "+l, width))
	}
	return out
}

// paneText is how wide the reading pane's text runs: long lines are hard to read.
func paneText(width int) int { return max(10, min(width-6, 92)) }

// A sidePane is a side of the program, the board or the queue.
type sidePane interface {
	// body is its lists, or the screen it has open in their place, h lines.
	body(width, h int) []string
	// reader is what it has open to read, h lines, nil for nothing.
	reader(width, h int) []string
	// keys are the footer's: those of the moment, and those of the row under the cursor.
	keys() string
	statusLine() (text string, c Color, at time.Time)
}

// screen is a pane alone, height lines of width columns: what it has open to read takes the
// place of its lists.
func screen(p sidePane, width, height int) string {
	footer := hints(p.keys(), width-2)
	rows := max(0, height-len(footer)-1)
	lines := p.reader(width, rows)
	if lines == nil {
		lines = p.body(width, rows)
	}
	text, c, _ := p.statusLine()
	return frame(lines, footer, text, c, width, rows)
}

var (
	tagRe          = regexp.MustCompile(`^\[([^\]]*)\]\s*`)
	conventionalRe = regexp.MustCompile(`^(fix|feat|docs|chore|refactor|test|perf|build|ci|style|revert)(\([^)]*\))?!?:\s*`)
)

// displayTitle is a summary without the tags its row has no use for: a PR's "fix: [FLD-1885]",
// a ticket's "[Reports App][Tests]". What is left starts with a capital.
func displayTitle(s string) string {
	whole := strings.TrimSpace(s)
	t := whole
	for {
		next := strings.TrimSpace(tagRe.ReplaceAllString(conventionalRe.ReplaceAllString(t, ""), ""))
		if next == t {
			break
		}
		t = next
	}
	if t == "" {
		return whole
	}
	if t != whole {
		r := []rune(t)
		r[0] = unicode.ToUpper(r[0])
		t = string(r)
	}
	return t
}

// titleTags are the bracketed tags a summary starts with, after a conventional commit's type.
func titleTags(s string) []string {
	var out []string
	t := conventionalRe.ReplaceAllString(strings.TrimSpace(s), "")
	for m := tagRe.FindStringSubmatch(t); m != nil; m = tagRe.FindStringSubmatch(t) {
		if m[1] != "" {
			out = append(out, m[1])
		}
		t = t[len(m[0]):]
	}
	return out
}
