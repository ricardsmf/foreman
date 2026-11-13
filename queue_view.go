package main

// The queue's screen: its rows, the detail under them and the keys.

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func (q *queueModel) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.ReportFocus = true
	v.WindowTitle = "queue"
	if n := q.needYou(); n > 0 {
		v.WindowTitle = fmt.Sprintf("queue (%d)", n)
	}
	v.SetContent(q.content())
	return v
}

// content is the queue as text, q.height lines of q.width columns; "" before it has a size.
func (q *queueModel) content() string {
	if q.width == 0 {
		return ""
	}
	inner := q.width - 2
	body := q.render(inner, q.height-1)
	if q.help {
		body = renderHelp("queue", queueHelp, inner, q.height-1, &q.helpScroll)
	}
	return frame(body, q.status, q.statusColor, inner)
}

func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

var groupNames = map[group]string{needsYou: "needs you", toReview: "review", working: "working", idle: "idle", closed: "closed"}

// itemLabel is the item's coloured mark and its state.
func itemLabel(it queueItem) (string, string) {
	if it.Kind == prItem {
		if !it.urgent() {
			return fg(cGreen, "✓"), fg(cGreen, it.Pr.Status)
		}
		return fg(cRed, "✗"), fg(cRed, it.Pr.Status)
	}
	what := "done"
	if it.review() {
		what = "reviewed"
	}
	if it.Report == "" {
		what = "no report"
	}
	if it.Orchestrator {
		what = "waiting for you"
	}
	if it.Watcher {
		what = "between passes"
		if it.Polled {
			what = "waits for a PR to change"
		}
		if it.Agent == "" {
			what = "not running, enter starts it"
		}
	}
	switch {
	case it.Question != nil:
		return fg(cRed, "●"), fg(cRed, "asks you")
	case it.Agent == "working":
		return fg(cYellow, "●"), fg(cYellow, "working")
	case it.Agent == "blocked" && it.Waits == "auth":
		return fg(cRed, "●"), fg(cRed, "needs a sign-in")
	case it.Agent == "blocked":
		return fg(cRed, "●"), fg(cRed, "asks you")
	case it.Agent == "error":
		return fg(cRed, "●"), fg(cRed, "failed")
	case it.Group == toReview && it.Agent == "":
		return fg(cGreen, "✓"), fg(cGreen, what) + fg(cOverlay, " (closed)")
	case it.Group == toReview:
		return fg(cGreen, "✓"), fg(cGreen, what)
	case it.Group == idle:
		return fg(cGreen, "○"), fg(cOverlay, what)
	}
	return fg(cOverlay, "◇"), fg(cOverlay, what)
}

func (q *queueModel) render(width, h int) []string {
	dot := fg(cSurface, " · ")
	need := fg(cGreen, "nothing needs you")
	if n := q.needYou(); n > 0 {
		need = bold(fg(cRed, fmt.Sprintf("%d need you", n)))
	}
	head := bold(fg(cMauve, "queue")) + dot + need
	for _, g := range []struct {
		g group
		c Color
	}{{working, cYellow}, {idle, cGreen}, {closed, cOverlay}} {
		if n := q.count(g.g); n > 0 {
			head += dot + fg(g.c, fmt.Sprintf("%d %s", n, groupNames[g.g]))
		}
	}
	if q.prsLoading {
		head += fg(cYellow, "  loading PRs…")
	}
	out := []string{head}

	// The keys of every moment, and those the row under the cursor adds; ? lists the rest.
	keys := "↑↓ pick · enter go/resume · f reply · x reviewed · d diff · ? all keys"
	if it, ok := q.current(); ok && it.Fold > 0 {
		keys = "↑↓ pick · enter list them · ? all keys"
	} else if ok && pressable(it) && q.seen[detailKey(it)] != "" {
		keys = "y press enter · n press esc · " + keys
	}
	if q.showClosed {
		keys += " · c fold closed"
	}
	if q.feedback != nil {
		keys = "type the reply · enter send · esc cancel"
	}
	footer := hints(keys, width)
	listRows := max(3, min(len(q.items), (h-2-len(footer))*40/100))
	detailRows := h - listRows - 2 - len(footer)
	q.top = scrollTo(q.top, q.cursor, listRows, len(q.items))

	keyW := 8
	for _, it := range q.items {
		keyW = max(keyW, len(it.Key))
	}
	keyW = min(22, keyW)
	// Ages line up in a column of their own at the right edge.
	ageW := 0
	for _, it := range q.items {
		ageW = max(ageW, len(age(it.When)))
	}
	for n := q.top; n < q.top+listRows; n++ {
		if n >= len(q.items) {
			empty := ""
			if n == 0 {
				empty = fg(cOverlay, "  no agents dispatched")
			}
			out = append(out, empty)
			continue
		}
		it := q.items[n]
		sel := n == q.cursor
		line := " "
		if sel {
			line = fg(cMauve, "▌")
		}
		// The group's name on its first row, and on the top row when the group starts above it.
		name := ""
		if n == q.top || q.items[n-1].Group != it.Group {
			name = groupNames[it.Group]
		}
		groupColor := cOverlay
		if it.pending() {
			groupColor = cMauve
		}
		line += fg(groupColor, fit(name, 9)) + " "
		if it.Fold > 0 {
			row := line + fg(cOverlay, "▸") + " " + fg(cSubtext, fmt.Sprintf("%d closed", it.Fold)) + fg(cOverlay, "  c lists them")
			if sel {
				row = selected(row, width)
			}
			out = append(out, row)
			continue
		}
		mark, what := itemLabel(it)
		// What a working agent says it is doing tells more than that it works.
		if it.Agent == "working" && it.Question == nil && it.Doing != "" {
			what = fg(cYellow, ansi.Truncate(it.Doing, max(16, width/3), "…"))
		}
		var ev []string
		if it.Shots > 0 {
			ev = append(ev, fmt.Sprintf("▣ %d", it.Shots))
		}
		if it.Videos > 0 {
			ev = append(ev, "▶ video")
		}
		meta := what
		if len(ev) > 0 {
			meta += "  " + fg(cBlue, strings.Join(ev, " "))
		}
		if ageW > 0 {
			meta += "  " + fg(cOverlay, fmt.Sprintf("%*s", ageW, age(it.When)))
		}
		keyColor, title := cOverlay, fg(cSubtext, it.Title)
		if it.Group == closed {
			title = fg(cOverlay, it.Title)
		}
		if sel {
			keyColor, title = cText, bold(fg(cText, it.Title))
		}
		left := line + mark + " " + fg(keyColor, fit(it.Key, keyW)) + "  "
		titleW := width - ansi.StringWidth(left) - ansi.StringWidth(meta) - 2
		row := left + fit(title, max(0, titleW)) + "  " + meta
		if sel {
			row = selected(row, width)
		}
		out = append(out, row)
	}

	var detail []string
	if q.feedback != nil {
		verb := "Reply to "
		if q.feedback.answer {
			verb = "Answer "
		}
		detail = append(detail, bold(fg(cMauve, verb+q.feedback.key)), "")
		for _, l := range wrap(cursorText(q.feedback.text, q.feedback.back), width-2) {
			detail = append(detail, "  "+l)
		}
	} else if it, ok := q.current(); ok {
		lines := q.details[detailKey(it)]
		if lines == nil {
			lines = []string{"# " + it.Key + ": " + it.Title, "", fg(cOverlay, "Loading…")}
		}
		detail = renderMarkdown(lines, width)
	}
	shown := window(detail, &q.scroll, detailRows)
	out = append(out, divider(width, q.scroll, detailRows, len(detail)))
	return append(append(out, shown...), footer...)
}
