package main

import (
	"charm.land/lipgloss/v2"
	"cmp"
	"fmt"
	"image/color"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The palette: the terminal's own hues, so the board follows its theme, and for the greys, which
// a palette of sixteen doesn't hold, shades between the terminal's background and its text (see
// setTheme). cText is the terminal's text colour.
var (
	cText   color.Color = lipgloss.NoColor{}
	cMauve  color.Color = lipgloss.Magenta
	cBlue   color.Color = lipgloss.Blue
	cYellow color.Color = lipgloss.Yellow
	cGreen  color.Color = lipgloss.Green
	cRed    color.Color = lipgloss.Red
	cCode   color.Color = lipgloss.Cyan
)

// The greys, and the selected row's background, until the terminal says what its colours are:
// Catppuccin Macchiato's.
var (
	cSubtext   color.Color = lipgloss.Color("#a5adcb")
	cOverlay   color.Color = lipgloss.Color("#8087a2")
	cSurface   color.Color = lipgloss.Color("#494d64")
	bgSelected color.Color = lipgloss.Color("#363a4f")
	// bgPanel is the reading pane's surface and a card's, a shade off the background.
	bgPanel color.Color = lipgloss.Color("#2b2e42")
)

// greyShare is where each grey sits between Catppuccin Macchiato's background and its text, in the
// order setTheme sets them: its contrast against the background as a power of the text's.
var greyShare = func() []float64 {
	base := lipgloss.Color("#24273a")
	top := math.Log(contrast(lipgloss.Color("#cad3f5"), base))
	var out []float64
	for _, c := range []color.Color{cSubtext, cOverlay, cSurface, bgSelected, bgPanel} {
		out = append(out, math.Log(contrast(c, base))/top)
	}
	return out
}()

// setTheme derives the greys from the terminal's own background and text colours: each is the
// first shade on the way from the background to the text whose contrast sits where Catppuccin's
// grey's does below its text. Measured in contrast rather than mixed by share of RGB, a light
// theme's greys read as well as a dark one's, and a soft theme's keep their steps below its text.
func setTheme(text, background color.Color) {
	shades := lipgloss.Blend1D(256, background, text)
	greys := make([]color.Color, len(greyShare))
	for n, share := range greyShare {
		want := math.Pow(contrast(text, background), share)
		greys[n] = text
		for _, s := range shades {
			if contrast(s, background) >= want {
				greys[n] = s
				break
			}
		}
	}
	cSubtext, cOverlay, cSurface, bgSelected, bgPanel = greys[0], greys[1], greys[2], greys[3], greys[4]
}

// contrast is WCAG's contrast ratio between two colours, from 1 for the same to 21 for black and
// white.
func contrast(a, b color.Color) float64 {
	lum := func(c color.Color) float64 {
		r, g, b, _ := c.RGBA()
		lin := func(v uint32) float64 {
			s := float64(v) / 0xffff
			if s <= 0.03928 {
				return s / 12.92
			}
			return math.Pow((s+0.055)/1.055, 2.4)
		}
		return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
	}
	la, lb := lum(a), lum(b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
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

// styled is s in style, which carries on past the styles s holds: Lip Gloss ends each with a reset
// of every attribute, so style opens again after each one.
func styled(style lipgloss.Style, s string) string {
	open, _, _ := strings.Cut(style.Render("x"), "x")
	if open == "" {
		return s
	}
	return style.Render(strings.ReplaceAll(s, ansi.ResetStyle, ansi.ResetStyle+open))
}

var plainStyle = lipgloss.NewStyle().TabWidth(lipgloss.NoTabConversion)

func fg(c color.Color, s string) string { return styled(plainStyle.Foreground(c), s) }
func bold(s string) string              { return styled(plainStyle.Bold(true), s) }

func fit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	cut := ansi.Truncate(text, width, "…")
	return cut + strings.Repeat(" ", max(0, width-ansi.StringWidth(cut)))
}

// selected is a list's row under the cursor, on a background across the whole width.
func selected(line string, width int) string {
	return paint(bgSelected, line, width)
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
			line += "   "
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

// markdownLine styles a line in greys and weights: colour is for states.
func markdownLine(l string) string {
	if strings.HasPrefix(l, "# ") {
		return bold(inline(l[2:]))
	}
	if subheading.MatchString(l) {
		return bold(fg(cSubtext, plain(subheading.ReplaceAllString(l, ""))))
	}
	return inline(l)
}

// inline styles a line's bold and code, and keeps its links' text.
func inline(l string) string {
	l = boldRe.ReplaceAllStringFunc(unlink(l), func(m string) string { return bold(m[2 : len(m)-2]) })
	// Code recedes a shade instead of taking a hue: a report is half file names.
	return codeRe.ReplaceAllStringFunc(l, func(m string) string { return fg(cSubtext, m[1:len(m)-1]) })
}

// wrap breaks s into lines width wide. A style that runs past a line's end is closed there and
// opened again on the next, so a line can sit beside another column.
func wrap(s string, width int) []string {
	return strings.Split(lipgloss.Wrap(s, max(1, width), ""), "\n")
}

// wrapN wraps s to at most n lines, the last one ending in an ellipsis when there was more.
func wrapN(s string, width, n int) []string {
	lines := wrap(s, width)
	if len(lines) <= n {
		return lines
	}
	lines = lines[:n]
	lines[n-1] = ansi.Truncate(lines[n-1], max(0, width-2), "") + " …"
	return lines
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
			mark := prefix
			if bullet := strings.TrimLeft(prefix, " "); strings.HasPrefix(bullet, "- ") || strings.HasPrefix(bullet, "* ") {
				mark = fg(cOverlay, strings.Replace(strings.Replace(prefix, "- ", "· ", 1), "* ", "· ", 1))
			}
			for n, w := range wrap(markdownLine(l[len(prefix):]), width-indent) {
				if n == 0 {
					out = append(out, mark+w)
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

// frame is the screen: rows lines of the body, then the footer's keys and the status line.
func frame(body, footer []string, status string, statusColor color.Color, width, rows int) string {
	lines := make([]string, 0, rows+len(footer)+1)
	for _, l := range tall(body, rows) {
		lines = append(lines, fit(l, width))
	}
	for _, l := range footer {
		lines = append(lines, " "+fit(l, width-1))
	}
	if status != "" {
		status = fg(statusColor, status)
	}
	return strings.Join(append(lines, " "+fit(status, width-1)), "\n")
}

// header names what the board lists, and for whom; extra says what it shows of it.
func (m *model) header(extra string) string {
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
	noun, scope := "Tickets", strings.TrimSuffix(m.title, " open tickets")
	if isPrs {
		noun, scope = "Pull requests", ""
	}
	return bold(noun) + fg(cOverlay, "  "+strings.Join(nonEmpty(scope, who, extra), " · "))
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
	return screen(m, m.width, m.height)
}

// body is the board's list, or the screen it has open in its place.
func (m *model) body(width, h int) []string {
	switch {
	case m.help:
		return indent(renderHelp("foreman", boardHelp, width-2, h, &m.helpScroll), 1)
	case m.cleanup != nil:
		return indent(m.renderCleanup(width-2, h), 1)
	case m.form != nil:
		return indent(m.renderForm(width-2, h), 1)
	}
	return m.renderBoard(width, h)
}

// reader is the reading pane of the row under the cursor, or the picker that changes it.
func (m *model) reader(width, h int) []string {
	if m.help || m.form != nil || m.cleanup != nil {
		return nil
	}
	tw := paneText(width)
	if m.picker != nil {
		return panel(m.renderPicker(tw, h-1), width, h)
	}
	r, ok := m.current()
	if m.reading == 0 || !ok {
		return nil
	}
	if m.reading == readBrief {
		return panel(m.rowBrief(r, tw, h-2), width, h)
	}
	detail := m.details[r.Key]
	if detail == nil {
		detail = []string{fg(cOverlay, "Loading…")}
	}
	return panel(readAllOf(r.Key, renderMarkdown(detail, tw), &m.detailScroll, tw, h-2), width, h)
}

func (m *model) statusLine() (string, color.Color, time.Time) {
	return m.status, m.statusColor, m.statusAt
}

// keys are the footer's: at most five for the moment and the row under the cursor, then ?.
func (m *model) keys() string {
	switch {
	case m.help:
		return "↑↓ scroll · any other key back"
	case m.cleanup != nil && m.cleanup.busy:
		return "removing…"
	case m.cleanup != nil:
		return "↑↓ pick · space toggle · a all/none · enter remove (branches stay; agent sessions close) · esc back"
	case m.form != nil:
		return m.formKeys()
	case m.picker != nil:
		return m.pickerKeys()
	case m.reading == readAll:
		return "^u ^d scroll · ← back · ↑↓ the next one · enter dispatch · ? keys"
	}
	isPrs := m.view == prs
	who := "^o everyone"
	switch {
	case isPrs:
		who = "^o requested/mine"
	case !m.mine:
		who = "^o mine"
	}
	row, ok := m.current()
	if !ok {
		keys := []string{"tab PRs", who, "^t task", "^r reload"}
		if isPrs {
			keys[0] = "tab tickets"
		}
		if m.query != "" {
			keys = append([]string{"esc clear the filter"}, keys[:3]...)
		}
		return strings.Join(append(keys, "? keys"), " · ")
	}
	enter := "enter dispatch"
	if isPrs {
		enter = "enter review"
	}
	keys := []string{enter, "→ read", who, "^t task"}
	if session := m.state.Dispatched[m.agentKey(row.Key)].Session; session != "" && m.agents[session] != "" {
		keys = []string{"^s go to its agent", enter + " again", "→ read", who}
	}
	if m.reading == readBrief {
		keys[slices.Index(keys, "→ read")] = "→ the whole text"
		keys = append(keys[:3], "← back")
	}
	return strings.Join(append(keys, "? keys"), " · ")
}

// A helpKey is a line of the key list: a key and what it does, or with no key, a heading.
type helpKey struct{ key, what string }

var boardHelp = []helpKey{
	{"", "The list"},
	{"↑↓  ^p ^n", "pick a row; pgup and pgdn move ten"},
	{"type, paste", "filter: every word must match"},
	{"esc", "close the reading pane, clear the filter, then quit"},
	{"tab", "tickets or open PRs"},
	{"^o", "mine (PRs: review requested, mine, everyone)"},
	{"^a", "cycle the assignee (PRs: the author)"},
	{"→", "read the row: its brief, then its whole text (where the filter has nothing to the cursor's right)"},
	{"←", "back from the whole text to the brief, then to the list"},
	{"^u  ^d", "scroll the description's whole text"},
	{"^r", "reload"},
	{"", "The row"},
	{"enter", "dispatch an agent on it (PRs: a review, or a fix)"},
	{"^s", "go to its agent's Rex session"},
	{"^b", "open it in a browser beside the board"},
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
	{"◆", "dispatched: red waiting for you or failed, yellow working, grey idle or done"},
	{"◇", "dispatched, its session closed"},
}

var queueHelp = []helpKey{
	{"", "The list"},
	{"↑↓  j k", "pick a row"},
	{"space  →", "read the row: its brief, then its whole text, which space scrolls"},
	{"←  esc", "back to the brief; close the reading pane"},
	{"^u  ^d", "scroll the whole text"},
	{"c", "show or fold the closed agents"},
	{"^r", "reload"},
	{"tab", "the next view: the queue, today, the stats"},
	{"q", "close the queue: the board alone (^z brings it back beside it)"},
	{"^l", "the board"},
	{"^z", "the queue alone, or beside the board"},
	{"", "The row"},
	{"enter", "go to the agent's session; resume a closed agent; start a stopped watcher"},
	{"f", "type a reply into the agent's pane, or answer its question: one it left resumes a closed agent"},
	{"y  n", "on an agent that waits on a prompt: press enter, or esc, in its pane"},
	{"1-9  a", "on a question with choices: answer with one, or with the recommended one; confirm what the agent went on with"},
	{"x", "mark reviewed, confirming what the agent went on with; set aside a question it left"},
	{"d", "the worktree's diff"},
	{"r", "review that diff: comment on its lines, and send the comments to the agent"},
	{"m", "on a PR that conflicts: have its agent merge the base in, or open the form for a fix that does"},
	{"t", "on a PR with review threads to answer: have its agent address them, or open the form for a fix that does"},
	{"v", "show the evidence in a browser beside the board"},
	{"p", "open a port the agent's session listens on in a browser beside the board; again for the next"},
	{"o", "open the ticket or PR in a browser beside the board"},
	{"O", "open the ticket or PR in the system browser"},
	{"K K", "close the agent's session, or drop a closed agent from the queue"},
	{"?", "this list"},
	{"", "The diff under review"},
	{"↑↓  j k", "pick a line; ^u and ^d move ten, g and G to the ends, n and p by hunk"},
	{"c  enter", "comment on the line, or change its comment"},
	{"x", "drop the line's comment"},
	{"s", "send the comments to the agent as one reply"},
	{"esc", "back to the queue; twice when comments are unsent"},
	{"", "Today"},
	{"enter", "go to the agent's session; on a ticket up next, the board's dispatch form"},
	{"o  O", "open the ticket or PR beside the board, or in the system browser"},
	{"", "Stats"},
	{"←→  h l", "count today, the last 7 days or the last 30"},
	{"g", "a row per dispatch, or per repo"},
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
			out = append(out, "", fg(cSubtext, k.what))
			continue
		}
		for n, l := range wrap(k.what, width-keyW-4) {
			key := ""
			if n == 0 {
				key = k.key
			}
			out = append(out, "  "+fg(cSubtext, fit(key, keyW))+"  "+fg(cOverlay, l))
		}
	}
	rows := max(0, h-2)
	shown := window(out[1:], scroll, rows)
	head := []string{bold(title) + fg(cOverlay, "  keys"), divider(width, *scroll, rows, len(out)-1)}
	return append(head, shown...)
}

// boardGroup is the heading a row sits under: a ticket's status, or which of the PRs it is.
func boardGroup(r Row) (string, color.Color) {
	if p := r.Pr; p != nil {
		switch {
		case p.Requested:
			return "review requested", cText
		case p.Mine:
			return "yours", cText
		}
		return "others", cText
	}
	tone := cText
	if r.Category == "Blocked" || strings.Contains(strings.ToLower(r.Status), "blocked") {
		tone = cRed
	}
	return strings.ToLower(cmp.Or(r.Status, "no status")), tone
}

// rowPeek is a few lines of the row's description, for under it while no reading pane shows it.
func (m *model) rowPeek(r Row, width int) []string {
	p := prose(descriptionOf(m.details[r.Key]))
	if len(p) == 0 {
		return nil
	}
	return wrapN(strings.Join(p, "  "), width, 3)
}

func (m *model) renderBoard(width, h int) []string {
	items := m.visible()
	isPrs := m.view == prs
	total := fmt.Sprint(len(m.rows()))
	if (isPrs && m.prsTruncated) || (!isPrs && m.truncated) {
		total += "+"
	}
	count := fmt.Sprintf("%d of %s", len(items), total)
	if m.loading() {
		count = "loading…  " + count
	}
	out := []string{lr(" "+m.header(""), fg(cOverlay, count)+" ", width)}
	if m.query != "" {
		out = append(out, " "+fg(cMauve, "› ")+cursorText(m.query, m.queryBack))
	}
	out = append(out, "")
	rows := max(1, h-len(out))
	if len(items) == 0 {
		empty := ""
		switch {
		case m.loading():
		case m.query != "" && isPrs:
			empty = "no PR matches"
		case m.query != "":
			empty = "no ticket matches"
		case isPrs:
			empty = "no open PRs"
		default:
			empty = "no tickets"
		}
		return append(out, tall([]string{"   " + fg(cOverlay, empty)}, rows)...)
	}

	keyW, whoW, statusW := 7, 0, 0
	for _, i := range items {
		keyW = max(keyW, len(i.Key))
		whoW = max(whoW, ansi.StringWidth(cmp.Or(i.Assignee, "unassigned")))
		if i.Pr != nil && !i.Pr.Requested {
			statusW = max(statusW, ansi.StringWidth(i.Status))
		}
	}
	keyW, whoW, statusW = min(18, keyW), min(14, whoW), min(12, statusW)
	// A list filtered to one person needn't name them on every row.
	showWho := m.assignee == nil && !(isPrs && m.prFilter == "mine") && !(!isPrs && m.mine)
	entries := make([]listEntry, len(items))
	for n, r := range items {
		sel := n == m.cursor && !m.unfocused
		right := ""
		if showWho {
			whoColor := cOverlay
			if r.isMine() {
				whoColor = cSubtext
			}
			right += fg(whoColor, fit(cmp.Or(r.Assignee, "unassigned"), whoW)) + "  "
		}
		// A PR's state is its own, where a ticket's is the heading it sits under.
		if statusW > 0 {
			status, c := "", cOverlay
			if r.Pr != nil && !r.Pr.Requested {
				status = r.Status
			}
			switch r.Category {
			case "Blocked":
				c = cRed
			case "Done":
				c = cGreen
			}
			right += fg(c, fit(status, statusW)) + "  "
		}
		right += fg(cOverlay, fit(r.Key, keyW))
		title := displayTitle(r.Summary)
		text := fg(cSubtext, title)
		if sel {
			text = bold(fg(cText, title))
		}
		lines := []string{row(sel, m.dispatchMark(m.agentKey(r.Key)), text, right, width)}
		if sel && m.reading == 0 && m.picker == nil {
			lines = append(lines, peek(m.rowPeek(r, width-6), 3, width)...)
		}
		group, tone := boardGroup(r)
		entries[n] = listEntry{group, tone, lines}
	}
	return append(out, groupedList(entries, m.cursor, &m.top, rows)...)
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
	header, heading := m.header(verb+" "+f.row.Key), bold(f.row.Key)+" "+fg(cSubtext, displayTitle(f.row.Summary))
	if f.task {
		header, heading = m.header("new task"), bold("New task")
		if repo := m.formRepo(); repo != "" {
			heading += fg(cOverlay, " in "+filepath.Base(repo))
		} else if len(m.repoChoices()) > 0 {
			heading += fg(cOverlay, " with "+noRepoChoice)
		}
	}
	return m.renderFormFields([]string{header, "", heading, fg(cSurface, strings.Repeat("─", width))}, width, h)
}

// formKeys are the dispatch form's.
func (m *model) formKeys() string {
	f := m.form
	if f.busy {
		return "dispatching…"
	}
	verb := "dispatch"
	if f.row.Pr != nil {
		verb = "review"
		if f.fix {
			verb = "fix"
		}
	}
	fieldKeys := []string{"←→ agent", "space toggle", "space toggle", "←→ repo", "type · alt+enter newline"}[f.field]
	return fmt.Sprintf("tab/↑↓ field · %s · ^e editor · enter %s · esc back", fieldKeys, verb)
}

// renderFormFields draws the form's fields under its heading, out.
func (m *model) renderFormFields(out []string, width, h int) []string {
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
			name, note := tilde(choices[n]), ""
			if choices[n] == "" {
				name, note = noRepoChoice, " "+fg(cOverlay, "(a scratch directory, for a question or work outside the repos)")
			}
			if on {
				name = bold(name)
			} else {
				name = fg(cSubtext, name)
			}
			out = append(out, "    "+radio(on)+" "+name+note)
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
		if room := max(2, h-len(out)); len(lines) > room {
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

	return tall(out, h)
}

// agentKey is the dispatch a row's mark and ^s follow: its own, else, for a PR not dispatched for
// review, its fix.
func (m *model) agentKey(key string) string {
	if m.state.Dispatched[key].Dir == "" && strings.Contains(key, "#") && m.state.Dispatched[fixKey(key)].Dir != "" {
		return fixKey(key)
	}
	return key
}

// dispatchMark is the board's dispatch column: ◆ red when the agent waits on me or failed, yellow
// while it works, grey when it rests, and ◇ when its session is closed.
func (m *model) dispatchMark(key string) string {
	if m.state.Dispatched[key].Dir == "" {
		return " "
	}
	session := m.state.Dispatched[key].Session
	switch m.agents[session] {
	case "blocked", "error":
		return fg(cRed, "◆")
	case "working":
		return fg(cYellow, "◆")
	case "done", "idle", "open":
		return fg(cSubtext, "◆")
	}
	// No session recorded (older dispatches) or it was closed.
	return fg(cOverlay, "◇")
}
