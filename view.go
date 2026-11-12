package main

import (
	"cmp"
	"fmt"
	"image/color"
	"path/filepath"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A Color is an SGR foreground: the terminal's palette for hues, so the board follows its theme,
// and for the greys, which a palette of sixteen doesn't hold, shades between the terminal's
// background and its text (see setTheme).
type Color string

const (
	cText   Color = "39"
	cMauve  Color = "35"
	cBlue   Color = "34"
	cYellow Color = "33"
	cGreen  Color = "32"
	cRed    Color = "31"
	cCode   Color = "36"
)

// The greys, and the selected row's background, until the terminal says what its colours are:
// Catppuccin Macchiato's.
var (
	cSubtext   Color = "38;2;165;173;203"
	cOverlay   Color = "38;2;128;135;162"
	cSurface   Color = "38;2;73;77;100"
	bgSelected       = "48;2;54;58;79"
)

// setTheme derives the greys from the terminal's own background and text colours: each sits a
// share of the way from the background to the text, the shares Catppuccin's greys have.
func setTheme(text, background color.Color) {
	fr, fgc, fb, _ := text.RGBA()
	br, bgc, bb, _ := background.RGBA()
	shade := func(share float64) string {
		mix := func(b, f uint32) int { return int((float64(b)+(float64(f)-float64(b))*share)/257 + 0.5) }
		return fmt.Sprintf("%d;%d;%d", mix(br, fr), mix(bgc, fgc), mix(bb, fb))
	}
	cSubtext, cOverlay, cSurface = Color("38;2;"+shade(0.78)), Color("38;2;"+shade(0.55)), Color("38;2;"+shade(0.22))
	bgSelected = "48;2;" + shade(0.11)
}

// A theme collects the terminal's answers about its colours; the greys follow once both are in.
type theme struct{ text, background color.Color }

// requestTheme asks the terminal for its text and background colours.
func requestTheme() tea.Cmd {
	return tea.Batch(tea.RequestForegroundColor, tea.RequestBackgroundColor)
}

// take applies the terminal's answer, and says whether msg was one.
func (t *theme) take(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case tea.ForegroundColorMsg:
		t.text = msg.Color
	case tea.BackgroundColorMsg:
		t.background = msg.Color
	default:
		return false
	}
	if t.text != nil && t.background != nil {
		setTheme(t.text, t.background)
	}
	return true
}

func fg(c Color, s string) string { return "\x1b[" + string(c) + "m" + s + "\x1b[39m" }
func bold(s string) string        { return "\x1b[1m" + s + "\x1b[22m" }

func fit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	cut := ansi.Truncate(text, width, "…")
	return cut + strings.Repeat(" ", max(0, width-ansi.StringWidth(cut)))
}

// selected is a list's row under the cursor, on a background across the whole width.
func selected(line string, width int) string {
	return "\x1b[" + bgSelected + "m" + fit(line, width) + "\x1b[49m"
}

// hints lays out a footer's keys, "key what · key what", on as many lines as width needs, breaking
// between keys so none is cut off.
func hints(text string, width int) []string {
	var lines []string
	line, used := "", 0
	for _, h := range strings.Split(text, " · ") {
		key, what, _ := strings.Cut(h, " ")
		w := ansi.StringWidth(h)
		switch {
		case used == 0:
		case used+3+w > width:
			lines = append(lines, line)
			line, used = "", 0
		default:
			line += fg(cSurface, " · ")
			used += 3
		}
		line += fg(cSubtext, key)
		if what != "" {
			line += " " + fg(cOverlay, what)
		}
		used += w
	}
	return append(lines, line)
}

// divider is the rule over a pane of rows lines scrolled to scroll, naming the lines shown when
// the text has more.
func divider(width, scroll, rows, total int) string {
	if total <= rows || rows <= 0 {
		return fg(cSurface, strings.Repeat("─", width))
	}
	pos := fmt.Sprintf(" %d–%d of %d ", scroll+1, min(total, scroll+rows), total)
	return fg(cSurface, strings.Repeat("─", max(0, width-ansi.StringWidth(pos)-2))) + fg(cOverlay, pos) + fg(cSurface, "──")
}

var (
	// A line's indent, with a list marker: what its wrapped lines hang under.
	listIndent = regexp.MustCompile(`^\s*(?:[-*+•]|\d+[.)])?\s+`)
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
	return codeRe.ReplaceAllStringFunc(l, func(m string) string { return fg(cCode, m[1:len(m)-1]) })
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

// renderMarkdown styles each line and wraps it to width: a list item's or an indented line's
// wrapped lines hang under its text, and a fenced block keeps its text as written.
func renderMarkdown(lines []string, width int) []string {
	var out []string
	fenced := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "```"):
			fenced = !fenced
			out = append(out, fg(cOverlay, l))
		case l == "":
			out = append(out, "")
		case fenced:
			out = append(out, wrap(fg(cSubtext, l), width)...)
		default:
			prefix := listIndent.FindString(l)
			indent := ansi.StringWidth(prefix)
			if indent > width/2 {
				prefix, indent = "", 0
			}
			for n, w := range wrap(markdownLine(l[len(prefix):]), width-indent) {
				if n == 0 {
					out = append(out, prefix+w)
				} else {
					out = append(out, strings.Repeat(" ", indent)+w)
				}
			}
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
	v.SetContent(m.content())
	return v
}

// content is the board as text, m.height lines of m.width columns; "" before it has a size.
func (m *model) content() string {
	if m.width == 0 {
		return ""
	}
	inner := m.width - 2
	var body []string
	switch {
	case m.help:
		body = renderHelp("foreman", boardHelp, inner, m.height-1, &m.helpScroll)
	case m.cleanup != nil:
		body = m.renderCleanup(inner, m.height-1)
	case m.form != nil:
		body = m.renderForm(inner, m.height-1)
	default:
		body = m.renderBoard(inner, m.height-1)
	}
	return frame(body, m.status, m.statusColor, inner)
}

// A helpKey is a line of the key list: a key and what it does, or with no key, a heading.
type helpKey struct{ key, what string }

var boardHelp = []helpKey{
	{"", "The list"},
	{"↑↓  ^p ^n", "pick a row; pgup and pgdn move ten"},
	{"type, paste", "filter: every word must match"},
	{"esc", "clear the filter, then quit"},
	{"tab", "tickets or open PRs"},
	{"^o", "mine (PRs: review requested, mine, everyone)"},
	{"^a", "cycle the assignee (PRs: the author)"},
	{"^u  ^d", "scroll the description"},
	{"^r", "reload"},
	{"", "The row"},
	{"enter", "dispatch an agent on it (PRs: a review, or a fix)"},
	{"^s", "go to its agent's Rex session"},
	{"^b", "open it in the browser"},
	{"^w", "set the ticket's status"},
	{"^y", "set the ticket's assignee"},
	{"", "Anywhere"},
	{"^t", "a task: work with no ticket or PR"},
	{"^g", "the orchestrator"},
	{"^x", "clean up the worktrees of finished work"},
	{"?", "this list, while the filter is empty"},
	{"^l", "the queue"},
	{"^z", "the board alone, or beside the queue"},
	{"", "Marks"},
	{"◆", "dispatched: yellow working, red waiting for you or failed, green idle or done"},
	{"◇", "dispatched, its session closed"},
}

var queueHelp = []helpKey{
	{"", "The list"},
	{"↑↓  j k", "pick a row"},
	{"^u  ^d  space", "scroll the report or screen"},
	{"c", "show or fold the closed agents"},
	{"^r", "reload"},
	{"q", "close the queue: the board alone (^z brings it back beside it)"},
	{"^l", "the board"},
	{"^z", "the queue alone, or beside the board"},
	{"", "The row"},
	{"enter", "go to the agent's session; resume a closed agent; start a stopped watcher"},
	{"f", "type a reply into the agent's pane, or answer its question"},
	{"y  n", "on an agent that waits on a prompt: press enter, or esc, in its pane"},
	{"x", "mark reviewed"},
	{"d", "the worktree's diff"},
	{"v", "open the screenshots and videos"},
	{"o", "open the ticket or PR in the browser"},
	{"K K", "close the agent's session, or drop a closed agent from the queue"},
	{"?", "this list"},
}

// renderHelp is the full list of keys, which the footer has no room for, scrolled to *scroll.
func renderHelp(title string, keys []helpKey, width, h int, scroll *int) []string {
	var out []string
	keyW := 0
	for _, k := range keys {
		keyW = max(keyW, ansi.StringWidth(k.key))
	}
	for _, k := range keys {
		if k.key == "" {
			out = append(out, "", bold(k.what))
			continue
		}
		for n, l := range wrap(k.what, width-keyW-4) {
			key := ""
			if n == 0 {
				key = k.key
			}
			out = append(out, "  "+fg(cSubtext, fit(key, keyW))+"  "+l)
		}
	}
	footer := hints("↑↓ scroll · any other key back", width)
	rows := max(0, h-2-len(footer))
	shown := window(out[1:], scroll, rows)
	head := []string{bold(fg(cMauve, title)) + fg(cSurface, " · ") + "keys", divider(width, *scroll, rows, len(out)-1)}
	return append(append(head, shown...), footer...)
}

func (m *model) renderBoard(width, h int) []string {
	items := m.visible()
	current, hasCurrent := m.current()

	isPrs := m.view == prs
	// The keys of every moment; ? lists the rest.
	keys := "↑↓ pick · enter dispatch · tab PRs · ^o mine · ^t task · ? all keys · esc quit"
	if isPrs {
		keys = "↑↓ pick · enter review · tab tickets · ^o requested/mine · ^t task · ? all keys · esc quit"
	}
	footer := hints(keys, width)
	if m.picker != nil {
		footer = hints(m.pickerKeys(), width)
		if m.picker.busy {
			footer = []string{fg(cOverlay, m.pickerKeys())}
		}
	}
	listRows := max(4, (h-3-len(footer))*45/100)
	detailRows := h - listRows - 3 - len(footer)
	m.top = scrollTo(m.top, m.cursor, listRows, len(items))

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
		hint = fg(cOverlay, " type to filter "+noun+": every word must match")
	}
	out = append(out, fg(cMauve, "› ")+cursorText(m.query, m.queryBack)+hint)

	keyW, statusW, whoW := 7, 6, 10
	for _, i := range items {
		keyW = max(keyW, len(i.Key))
		statusW = max(statusW, ansi.StringWidth(i.Status))
		whoW = max(whoW, ansi.StringWidth(cmp.Or(i.Assignee, "unassigned")))
	}
	keyW, statusW, whoW = min(24, keyW), min(14, statusW), min(18, whoW)
	// A list filtered to one person needn't name them on every row.
	showWho := m.assignee == nil && !(isPrs && m.prFilter == "mine") && !(!isPrs && m.mine)
	if !showWho {
		whoW = -2
	}
	sumW := width - keyW - statusW - whoW - 9
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
		line += m.dispatchMark(m.agentKey(i.Key)) + " "
		keyColor, summary := cOverlay, fg(cSubtext, fit(i.Summary, sumW))
		if sel {
			keyColor, summary = cText, bold(fg(cText, fit(i.Summary, sumW)))
		}
		cols := []string{fg(keyColor, fit(i.Key, keyW)), fg(statusColor, fit(i.Status, statusW))}
		if showWho {
			cols = append(cols, fg(whoColor, fit(cmp.Or(i.Assignee, "unassigned"), whoW)))
		}
		line += strings.Join(append(cols, summary), "  ")
		if sel {
			line = selected(line, width)
		}
		out = append(out, line)
	}

	if m.picker != nil {
		out = append(out, fg(cSurface, strings.Repeat("─", width)))
		out = append(out, m.renderPicker(width, detailRows)...)
		return append(out, footer...)
	}
	var detail []string
	if hasCurrent {
		if lines := m.details[current.Key]; lines != nil {
			detail = lines
		} else {
			detail = []string{fmt.Sprintf("# %s: %s", current.Key, current.Summary), "", fg(cOverlay, "Loading…")}
		}
	}
	text := renderMarkdown(detail, width)
	shown := window(text, &m.detailScroll, detailRows)
	out = append(out, divider(width, m.detailScroll, detailRows, len(text)))
	return append(append(out, shown...), footer...)
}

func (m *model) renderForm(width, h int) []string {
	f := m.form
	verb := "dispatch"
	if f.row.Pr != nil {
		verb = "review"
		if f.fix {
			verb = "fix"
		}
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
	fieldKeys := []string{"←→ agent", "space toggle", "space toggle", "←→ repo", "type · alt+enter newline"}[f.field]
	footer := hints(fmt.Sprintf("tab/↑↓ field · %s · ^e editor · enter %s · esc back", fieldKeys, verb), width)
	if f.busy {
		footer = []string{fg(cOverlay, "dispatching…")}
	}
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
	if isPr {
		p := f.row.Pr
		checkout := "    " + fg(cSubtext, tilde(fmt.Sprintf("%s-pr-%d", p.Checkout, p.Number))) + " " + fg(cOverlay, "(worktree at the PR head)")
		if f.fix {
			checkout = "    " + fg(cOverlay, "the worktree on ") + fg(cSubtext, p.Branch) + fg(cOverlay, ", else ") +
				fg(cSubtext, tilde(fmt.Sprintf("%s-fix-%d", p.Checkout, p.Number))) + " " + fg(cOverlay, "(a new one tracking origin's)")
		}
		out = append(out, "", "  "+bold("Options"),
			option(fieldWorktree, f.fix, "fix it", "(commit on its branch instead of reviewing: the review comments, or your context)"),
			"", "  "+bold("Checkout"), checkout)
	} else {
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
		text := f.context
		if f.field == fieldContext {
			text = cursorText(text, f.contextBack)
		}
		var lines []string
		for _, para := range strings.Split(text, "\n") {
			lines = append(lines, wrap(para, width-4)...)
		}
		// Keep the end, where the cursor is, in view when the text outgrows the screen.
		if room := max(2, h-len(footer)-len(out)); len(lines) > room {
			lines = append([]string{fg(cOverlay, fmt.Sprintf("… %d more lines", len(lines)-room+1))}, lines[len(lines)-room+1:]...)
		}
		for _, l := range lines {
			out = append(out, "    "+l)
		}
	} else if f.task {
		out = append(out, "    "+fg(cOverlay, "What to do: a change, a fix, a question to answer…"))
	} else if isPr && f.fix {
		out = append(out, "    "+fg(cOverlay, "What to fix; leave empty to address the unresolved review comments"))
	} else if isPr {
		out = append(out, "    "+fg(cOverlay, "What to focus on: risky areas, things you already know, questions to answer…"))
	} else {
		out = append(out, "    "+fg(cOverlay, "Anything the ticket doesn't say: approach, files to look at, constraints…"))
	}

	for len(out) < h-len(footer) {
		out = append(out, "")
	}
	return append(out[:max(0, h-len(footer))], footer...)
}

// agentKey is the dispatch a row's mark and ^s follow: its own, else, for a PR not dispatched for
// review, its fix.
func (m *model) agentKey(key string) string {
	if m.state.Dispatched[key] == "" && strings.Contains(key, "#") && m.state.Dispatched[fixKey(key)] != "" {
		return fixKey(key)
	}
	return key
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
