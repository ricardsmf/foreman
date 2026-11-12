package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Catppuccin Macchiato
type Color string

const (
	cText    Color = "202;211;245"
	cSubtext Color = "165;173;203"
	cOverlay Color = "128;135;162"
	cSurface Color = "73;77;100"
	cMauve   Color = "198;160;246"
	cBlue    Color = "138;173;244"
	cYellow  Color = "238;212;159"
	cGreen   Color = "166;218;149"
	cRed     Color = "237;135;150"
	cPeach   Color = "245;169;127"
)

func fg(c Color, s string) string { return "\x1b[38;2;" + string(c) + "m" + s + "\x1b[39m" }
func bold(s string) string        { return "\x1b[1m" + s + "\x1b[22m" }

func fit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	cut := ansi.Truncate(text, width, "…")
	return cut + strings.Repeat(" ", max(0, width-ansi.StringWidth(cut)))
}

var (
	subheading = regexp.MustCompile(`^#{2,} `)
	boldRe     = regexp.MustCompile(`\*\*(.+?)\*\*`)
	codeRe     = regexp.MustCompile("`([^`]+)`")
)

func markdownLine(l string) string {
	if strings.HasPrefix(l, "# ") {
		return bold(fg(cMauve, l[2:]))
	}
	if subheading.MatchString(l) {
		return bold(fg(cBlue, subheading.ReplaceAllString(l, "")))
	}
	l = boldRe.ReplaceAllStringFunc(l, func(m string) string { return bold(m[2 : len(m)-2]) })
	return codeRe.ReplaceAllStringFunc(l, func(m string) string { return fg(cPeach, m[1:len(m)-1]) })
}

func wrap(s string, width int) []string {
	return strings.Split(ansi.Wrap(s, max(1, width), ""), "\n")
}

// scrollTo is the list's first row, moved just far enough to show the cursor.
func scrollTo(top, cursor, rows, total int) int {
	if cursor < top {
		top = cursor
	}
	if cursor >= top+rows {
		top = cursor - rows + 1
	}
	return max(0, min(top, max(0, total-rows)))
}

// renderMarkdown styles each line and wraps it to width.
func renderMarkdown(lines []string, width int) []string {
	var out []string
	for _, l := range lines {
		if l == "" {
			out = append(out, "")
		} else {
			out = append(out, wrap(markdownLine(l), width)...)
		}
	}
	return out
}

// window is the rows lines from *scroll on, padded with empty ones. It keeps *scroll within the text.
func window(lines []string, scroll *int, rows int) []string {
	*scroll = max(0, min(*scroll, len(lines)-rows))
	var out []string
	for n := 0; n < rows; n++ {
		if at := *scroll + n; at < len(lines) {
			out = append(out, lines[at])
		} else {
			out = append(out, "")
		}
	}
	return out
}

// frame is the screen: the body padded to width, over the status line.
func frame(body []string, status string, statusColor Color, width int) string {
	lines := make([]string, 0, len(body)+1)
	for _, l := range body {
		lines = append(lines, " "+fit(l, width))
	}
	if status != "" {
		status = fg(statusColor, status)
	}
	return strings.Join(append(lines, " "+fit(status, width)), "\n")
}

func (m *model) header(extra string) string {
	dot := fg(cSurface, " · ")
	isPrs := m.view == prs
	who := "everyone"
	switch {
	case isPrs && m.prFilter == "requested":
		who = "review requested"
	case isPrs && m.prFilter == "mine", m.mine && !isPrs:
		who = "mine"
	case m.assignee != nil:
		who = cmp.Or(*m.assignee, "unassigned")
	}
	title := m.title
	if isPrs {
		title = "open PRs"
	}
	loading := ""
	if m.loading() {
		loading = fg(cYellow, "  loading…")
	}
	return bold(fg(cMauve, "foreman")) + dot + title + dot + fg(cYellow, who) + dot + fg(cOverlay, extra) + loading
}

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.ReportFocus = true
	if m.width == 0 {
		return v
	}
	inner := m.width - 2
	var body []string
	switch {
	case m.cleanup != nil:
		body = m.renderCleanup(inner, m.height-1)
	case m.form != nil:
		body = m.renderForm(inner, m.height-1)
	default:
		body = m.renderBoard(inner, m.height-1)
	}
	v.SetContent(frame(body, m.status, m.statusColor, inner))
	return v
}

func (m *model) renderBoard(width, h int) []string {
	items := m.visible()
	current, hasCurrent := m.current()

	listRows := max(4, (h-4)*45/100)
	detailRows := h - listRows - 4
	m.top = scrollTo(m.top, m.cursor, listRows, len(items))

	isPrs := m.view == prs
	total := fmt.Sprint(len(m.rows()))
	if (isPrs && m.prsTruncated) || (!isPrs && m.truncated) {
		total += "+"
	}
	out := []string{m.header(fmt.Sprintf("%d/%s", len(items), total))}
	hint := ""
	if m.query == "" {
		noun := "tickets"
		if isPrs {
			noun = "PRs"
		}
		hint = fg(cOverlay, " fuzzy search "+noun+"…")
	}
	out = append(out, fg(cMauve, "› ")+m.query+fg(cMauve, "█")+hint)

	keyW, statusW, whoW := 7, 6, 10
	for _, i := range items {
		keyW = max(keyW, len(i.Key))
		statusW = max(statusW, ansi.StringWidth(i.Status))
		whoW = max(whoW, ansi.StringWidth(cmp.Or(i.Assignee, "unassigned")))
	}
	keyW, statusW, whoW = min(24, keyW), min(14, statusW), min(18, whoW)
	sumW := width - keyW - statusW - whoW - 8
	for n := m.top; n < m.top+listRows; n++ {
		if n >= len(items) {
			empty := ""
			if n == 0 && !m.loading() {
				empty = fg(cOverlay, "  no tickets")
				if isPrs {
					empty = fg(cOverlay, "  no open PRs")
				}
			}
			out = append(out, empty)
			continue
		}
		i := items[n]
		sel := n == m.cursor
		statusColor := cOverlay
		switch i.Category {
		case "In Progress":
			statusColor = cYellow
		case "Done":
			statusColor = cGreen
		case "Blocked":
			statusColor = cRed
		}
		whoColor := cSubtext
		if i.Assignee == "" {
			whoColor = cOverlay
		} else if i.isMine() {
			whoColor = cMauve
		}
		line := " "
		if sel {
			line = fg(cMauve, "▌")
		}
		line += m.dispatchMark(i.Key)
		keyColor, summary := cOverlay, fg(cSubtext, fit(i.Summary, sumW))
		if sel {
			keyColor, summary = cText, bold(fg(cText, fit(i.Summary, sumW)))
		}
		line += strings.Join([]string{
			fg(keyColor, fit(i.Key, keyW)),
			fg(statusColor, fit(i.Status, statusW)),
			fg(whoColor, fit(cmp.Or(i.Assignee, "unassigned"), whoW)),
			summary,
		}, "  ")
		out = append(out, line)
	}

	out = append(out, fg(cSurface, strings.Repeat("─", width)))
	if m.picker != nil {
		out = append(out, m.renderPicker(width, detailRows)...)
		return append(out, fg(cOverlay, m.pickerKeys()))
	}
	var detail []string
	if hasCurrent {
		if lines := m.details[current.Key]; lines != nil {
			detail = lines
		} else {
			detail = []string{fmt.Sprintf("# %s: %s", current.Key, current.Summary), "", fg(cOverlay, "Loading…")}
		}
	}
	out = append(out, window(renderMarkdown(detail, width), &m.detailScroll, detailRows)...)

	if isPrs {
		out = append(out, fg(cOverlay, "↑↓ pick · enter review · tab tickets · ^o requested/mine · ^a author · ^u/^d scroll · ^b browser · ^s session · ^t task · ^g orchestrator · ^x clean up · ^r refresh · esc quit"))
	} else {
		out = append(out, fg(cOverlay, "↑↓ pick · enter dispatch · tab PRs · ^o mine · ^a assignee · ^w set status · ^y set assignee · ^u/^d scroll · ^b browser · ^s session · ^t task · ^g orchestrator · ^x clean up · ^r refresh · esc quit"))
	}
	return out
}

func (m *model) renderForm(width, h int) []string {
	f := m.form
	verb := "dispatch"
	if f.row.Pr != nil {
		verb = "review"
	}
	header, heading := m.header(verb+" "+f.row.Key), fg(cMauve, f.row.Key)+" "+bold(`"`+f.row.Summary+`"`)
	if f.task {
		header, heading = m.header("new task"), bold(fg(cMauve, "New task"))
		if repo := m.formRepo(); repo != "" {
			heading += fg(cOverlay, " in "+filepath.Base(repo))
		}
	}
	return m.renderFormFields([]string{header, heading, fg(cMauve, strings.Repeat("─", width))}, verb, width, h)
}

// renderFormFields draws the form's fields under its heading, out.
func (m *model) renderFormFields(out []string, verb string, width, h int) []string {
	f := m.form
	isPr := f.row.Pr != nil
	label := func(n int, text string) string {
		if f.field == n {
			return fg(cMauve, "› "+text)
		}
		return "  " + text
	}
	radio := func(on bool) string {
		if on {
			return fg(cGreen, "●")
		}
		return fg(cOverlay, "○")
	}

	out = append(out, label(fieldAgent, bold("Agent")))
	for n, a := range agents {
		name := a.Label
		if n == f.agent {
			name = bold(name)
		}
		out = append(out, "    "+radio(n == f.agent)+" "+fit(name, 16)+fg(cOverlay, a.Note))
	}
	if repo := m.formRepo(); hasSkills(repo) {
		note, color := "ships Claude Code skills", cOverlay
		if agents[f.agent].Label == agentPi {
			note, color = note+", which pi doesn't load", cYellow
		}
		out = append(out, "    "+fg(color, filepath.Base(repo)+" "+note))
	}
	if isPr {
		p := f.row.Pr
		out = append(out, "", "  "+bold("Checkout"),
			"    "+fg(cSubtext, tilde(fmt.Sprintf("%s-pr-%d", p.Checkout, p.Number)))+" "+fg(cOverlay, "(worktree at the PR head)"))
	} else {
		option := func(n int, on bool, name, note string) string {
			mark, box := "    ", "[ ]"
			if f.field == n {
				mark = "  " + fg(cMauve, "› ")
			}
			if on {
				box = fg(cGreen, "[x]")
			}
			return mark + box + " " + name + " " + fg(cOverlay, note)
		}
		out = append(out, "", "  "+bold("Options"),
			option(fieldWorktree, f.worktree, "git worktree", "(isolated checkout, parallel-safe)"),
			option(fieldPoteto, f.poteto, "poteto-mode", "(pstack playbooks and subagents: slower, more tokens)"), "")
		choices := m.repoChoices()
		f.repo = max(0, min(f.repo, len(choices)-1))
		filter := fg(cOverlay, "type to filter")
		if f.repoQuery != "" {
			filter = fg(cMauve, "/"+f.repoQuery)
		}
		out = append(out, label(fieldRepo, bold("Repo")+" "+filter))
		const rows = 6
		start := max(0, min(f.repo-2, len(choices)-rows))
		for n := start; n < min(len(choices), start+rows); n++ {
			on := n == f.repo
			name := fg(cSubtext, tilde(choices[n]))
			if on {
				name = bold(tilde(choices[n]))
			}
			out = append(out, "    "+radio(on)+" "+name)
		}
		if len(choices) == 0 {
			out = append(out, fg(cRed, "    no repo matches"))
		}
	}
	if f.task {
		out = append(out, "", label(fieldContext, bold("Task")+" "+fg(cOverlay, "(its first line names it)")))
	} else {
		out = append(out, "", label(fieldContext, bold("Extra context")+" "+fg(cOverlay, "(optional)")))
	}
	if f.context != "" || f.field == fieldContext {
		cursor := ""
		if f.field == fieldContext {
			cursor = "█"
		}
		var lines []string
		for _, para := range strings.Split(f.context+cursor, "\n") {
			lines = append(lines, wrap(para, width-4)...)
		}
		// Keep the end, where the cursor is, in view when the text outgrows the screen.
		if room := max(2, h-1-len(out)); len(lines) > room {
			lines = append([]string{fg(cOverlay, fmt.Sprintf("… %d more lines", len(lines)-room+1))}, lines[len(lines)-room+1:]...)
		}
		for _, l := range lines {
			out = append(out, "    "+l)
		}
	} else if f.task {
		out = append(out, "    "+fg(cOverlay, "What to do: a change, a fix, a question to answer…"))
	} else if isPr {
		out = append(out, "    "+fg(cOverlay, "What to focus on: risky areas, things you already know, questions to answer…"))
	} else {
		out = append(out, "    "+fg(cOverlay, "Anything the ticket doesn't say: approach, files to look at, constraints…"))
	}

	for len(out) < h-1 {
		out = append(out, "")
	}
	out = out[:max(0, h-1)]
	if f.busy {
		out = append(out, fg(cOverlay, "dispatching…"))
	} else {
		fieldKeys := []string{"←→ agent", "space toggle", "space toggle", "←→ repo", "type · alt+enter newline"}[f.field]
		fieldKeys += " · ^e editor"
		out = append(out, fg(cOverlay, fmt.Sprintf("tab/↑↓ field · %s · enter %s · esc back", fieldKeys, verb)))
	}
	return out
}

// dispatchMark is the board's dispatch column: ◆ coloured by what the agent's Rex session reports,
// ◇ when the session is closed.
func (m *model) dispatchMark(key string) string {
	if m.state.Dispatched[key] == "" {
		return " "
	}
	session := m.state.Sessions[key]
	switch m.agents[session] {
	case "blocked", "error":
		return fg(cRed, "◆")
	case "working":
		return fg(cYellow, "◆")
	case "done", "idle", "open":
		return fg(cGreen, "◆")
	}
	// No session recorded (older dispatches) or it was closed.
	return fg(cOverlay, "◇")
}
