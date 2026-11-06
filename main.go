// foreman [PROJECT | JQL]: Jira board with assignees over a detail pane; Enter opens a
// dispatch form that starts pi or Claude Code on the ticket in a git worktree, in a new Rex session.
// Tab switches to open PRs in the repos under ~/work/code (mine with review and CI status), where Enter dispatches a review.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const defaultProject = "FLD"

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

var projectRe = regexp.MustCompile(`(?i)^[A-Z][A-Z0-9]+$`)

func parseArgs(argv []string) (jql, title string) {
	arg := strings.TrimSpace(strings.Join(argv, " "))
	if arg != "" && !projectRe.MatchString(arg) {
		return arg, "custom JQL"
	}
	project := strings.ToUpper(cmp(arg, defaultProject))
	return fmt.Sprintf("project = %s AND statusCategory != Done ORDER BY updated DESC", project), project + " open tickets"
}

func openURL(url string) { _ = exec.Command("open", url).Start() }

// Row is a ticket or a PR in the board's common shape.
type Row struct {
	Key, Summary, Status, Category, Assignee string
	Pr                                       *Pr
	Issue                                    *Issue
}

func issueRow(i Issue) Row {
	return Row{i.Key, i.Summary, i.Status, i.Category, i.Assignee, nil, &i}
}

func prRow(p Pr) Row {
	return Row{p.Key, p.Summary, p.Status, p.Category, p.Assignee, &p, nil}
}

type viewKind int

const (
	tickets viewKind = iota
	prs
)

type form struct {
	row       Row
	field     int
	agent     int
	worktree  bool
	repo      int
	repoQuery string
	context   string
	busy      bool
}

// Messages from background work.
type (
	loadedMsg struct {
		which viewKind
		rows  []Row
		err   error
	}
	detailTickMsg struct{ seq int }
	detailMsg     struct {
		key   string
		lines []string
	}
	statusMsg struct {
		text  string
		color Color
	}
	dispatchedMsg struct {
		text string
		err  error
	}
	meMsg string
)

type model struct {
	jira  *Jira
	jql   string
	title string
	me    string

	width, height int
	view          viewKind
	issues        []Row
	prs           []Row
	prsLoaded     bool
	prFilter      string // "all" | "requested" | "mine"
	cursor, top   int
	query         string
	mine          bool
	assignee      *string // nil = everyone, "" = unassigned
	// Views with a load in flight. PRs load in the background from the start, so each view tracks its own.
	inFlight     map[viewKind]bool
	status       string
	statusColor  Color
	detailScroll int
	details      map[string][]string // nil value = loading
	detailSeq    int
	state        State
	repos        []string
	form         *form
}

func (m *model) loading() bool { return m.inFlight[m.view] }

func (m *model) say(text string, c Color) { m.status, m.statusColor = text, c }

func (m *model) rows() []Row {
	if m.view == prs {
		return m.prs
	}
	return m.issues
}

func (m *model) reload(which viewKind) tea.Cmd {
	if m.inFlight[which] {
		return nil
	}
	m.inFlight[which] = true
	jira, jql := m.jira, m.jql
	return func() tea.Msg {
		if which == prs {
			list, err := openPrs()
			rows := make([]Row, len(list))
			for n, p := range list {
				rows[n] = prRow(p)
			}
			return loadedMsg{which, rows, err}
		}
		list, err := jira.issues(context.Background(), jql)
		rows := make([]Row, len(list))
		for n, i := range list {
			rows[n] = issueRow(i)
		}
		return loadedMsg{which, rows, err}
	}
}

func (m *model) assignees() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.rows() {
		if !seen[r.Assignee] {
			seen[r.Assignee] = true
			out = append(out, r.Assignee)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if a == "" {
			return -1
		}
		if b == "" {
			return 1
		}
		return localeCompare(a, b)
	})
	return out
}

func (m *model) visible() []Row {
	words := strings.Fields(strings.ToLower(m.query))
	var out []Row
	for _, r := range m.rows() {
		if r.Pr != nil {
			if (m.prFilter == "requested" && !r.Pr.Requested) || (m.prFilter == "mine" && !r.Pr.Mine) {
				continue
			}
		} else if m.mine && r.Assignee != m.me {
			continue
		}
		if m.assignee != nil && r.Assignee != *m.assignee {
			continue
		}
		hay := strings.ToLower(fmt.Sprintf("%s %s %s %s", r.Key, r.Status, cmp(r.Assignee, "unassigned"), r.Summary))
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(hay, w) }) {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) current() (Row, bool) {
	items := m.visible()
	m.cursor = max(0, min(m.cursor, len(items)-1))
	if len(items) == 0 {
		return Row{}, false
	}
	return items[m.cursor], true
}

// scheduleDetail debounces loading the selected row's detail, so scrolling through the list doesn't fetch every row.
func (m *model) scheduleDetail() tea.Cmd {
	row, ok := m.current()
	if !ok || m.form != nil {
		return nil
	}
	if _, cached := m.details[row.Key]; cached {
		return nil
	}
	m.detailSeq++
	seq := m.detailSeq
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return detailTickMsg{seq} })
}

func (m *model) fetchDetail(row Row) tea.Cmd {
	m.details[row.Key] = nil
	jira := m.jira
	return func() tea.Msg {
		var md string
		var err error
		if row.Pr != nil {
			var v PrView
			if v, err = viewPr(*row.Pr); err == nil {
				md = prMarkdown(*row.Pr, v)
			}
		} else {
			md, err = jira.detail(context.Background(), row.Key)
		}
		if err != nil {
			return detailMsg{row.Key, []string{fg(cRed, fmt.Sprintf("Failed to load %s: %v", row.Key, err))}}
		}
		return detailMsg{row.Key, strings.Split(md, "\n")}
	}
}

func (m *model) repoChoices() []string {
	q := ""
	if m.form != nil {
		q = strings.ToLower(m.form.repoQuery)
	}
	var out []string
	for _, r := range m.repos {
		if q == "" || strings.Contains(strings.ToLower(tilde(r)), q) {
			out = append(out, r)
		}
	}
	return out
}

func project(key string) string { return strings.SplitN(key, "-", 2)[0] }

func (m *model) openForm(row Row) {
	m.form = &form{
		row:      row,
		agent:    max(0, slices.IndexFunc(agents, func(a Agent) bool { return a.Label == m.state.Agent })),
		worktree: true,
		repo:     max(0, slices.Index(m.repos, m.state.RepoByProject[project(row.Key)])),
	}
}

var prog *tea.Program

func (m *model) submit() tea.Cmd {
	f := m.form
	choices := m.repoChoices()
	repo := ""
	if f.repo < len(choices) {
		repo = choices[f.repo]
	}
	if (repo == "" && f.row.Pr == nil) || f.busy {
		return nil
	}
	f.busy = true
	if f.row.Pr == nil {
		m.state.RepoByProject[project(f.row.Key)] = repo
	}
	agent := agents[f.agent].Label
	m.state.Agent = agent
	_ = saveState(m.state)
	progress := func(line string) { prog.Send(statusMsg{line, cSubtext}) }
	jira, row, worktree, extra := m.jira, f.row, f.worktree, strings.TrimSpace(f.context)
	return func() tea.Msg {
		ctx := context.Background()
		var text string
		var err error
		if row.Pr != nil {
			text, err = dispatchReview(ctx, jira, ReviewDispatch{*row.Pr, agent, extra}, progress)
		} else {
			text, err = dispatch(ctx, jira, Dispatch{*row.Issue, agent, worktree, repo, extra}, progress)
		}
		if err != nil {
			err = fmt.Errorf("%s: %w", row.Key, err)
		}
		return dispatchedMsg{text, err}
	}
}

func (m *model) Init() tea.Cmd {
	jira := m.jira
	fetchMe := func() tea.Msg { return meMsg(jira.me(context.Background())) }
	return tea.Batch(m.reload(tickets), m.reload(prs), fetchMe)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case loadedMsg:
		delete(m.inFlight, msg.which)
		if msg.err != nil {
			name := "Tickets"
			if msg.which == prs {
				name = "PRs"
			}
			m.say(fmt.Sprintf("%s: %v", name, msg.err), cRed)
			break
		}
		if msg.which == prs {
			m.prs, m.prsLoaded = msg.rows, true
		} else {
			m.issues = msg.rows
		}
		// PR keys hold a `#`, ticket keys don't: keep the other view's cached details.
		for key := range m.details {
			if strings.Contains(key, "#") == (msg.which == prs) {
				delete(m.details, key)
			}
		}
		if msg.which == m.view {
			m.say("", cOverlay)
		}
	case detailTickMsg:
		if msg.seq != m.detailSeq || m.form != nil {
			return m, nil
		}
		if row, ok := m.current(); ok {
			if _, cached := m.details[row.Key]; !cached {
				return m, m.fetchDetail(row)
			}
		}
		return m, nil
	case meMsg:
		m.me = string(msg)
	case detailMsg:
		m.details[msg.key] = msg.lines
	case statusMsg:
		m.say(msg.text, msg.color)
	case dispatchedMsg:
		if msg.err != nil {
			if m.form != nil {
				m.form.busy = false
			}
			m.say(msg.err.Error(), cRed)
		} else {
			m.state = loadState()
			m.form = nil
			m.say(msg.text, cGreen)
		}
	case tea.KeyPressMsg:
		var cmd tea.Cmd
		if m.form != nil {
			cmd = m.formInput(msg)
		} else {
			cmd = m.boardInput(msg)
		}
		return m, tea.Batch(cmd, m.scheduleDetail())
	}
	return m, m.scheduleDetail()
}

func (m *model) boardInput(msg tea.KeyPressMsg) tea.Cmd {
	current, ok := m.current()
	move := func(to int) { m.cursor, m.detailScroll = max(0, to), 0 }
	text := msg.Key().Text
	switch k := msg.String(); {
	case k == "esc":
		if m.query == "" {
			return tea.Quit
		}
		m.query = ""
	case k == "ctrl+c":
		return tea.Quit
	case k == "up" || k == "ctrl+p":
		move(m.cursor - 1)
	case k == "down" || k == "ctrl+n":
		move(m.cursor + 1)
	case k == "pgup":
		move(m.cursor - 10)
	case k == "pgdown":
		move(m.cursor + 10)
	case k == "ctrl+u":
		m.detailScroll -= 10
	case k == "ctrl+d":
		m.detailScroll += 10
	case k == "tab":
		m.view = 1 - m.view
		m.assignee, m.query = nil, ""
		move(0)
		if m.view == prs && !m.prsLoaded {
			return m.reload(prs) // the startup load failed
		}
	case k == "ctrl+o":
		if m.view == prs {
			m.prFilter = map[string]string{"all": "requested", "requested": "mine", "mine": "all"}[m.prFilter]
		} else {
			m.mine = !m.mine && m.me != ""
		}
		m.assignee = nil
		move(0)
	case k == "ctrl+a":
		list := append([]*string{nil}, ptrs(m.assignees())...)
		at := slices.IndexFunc(list, func(s *string) bool {
			return (s == nil && m.assignee == nil) || (s != nil && m.assignee != nil && *s == *m.assignee)
		})
		m.assignee = list[(at+1)%len(list)]
		m.mine = false
		move(0)
	case k == "ctrl+b":
		if ok {
			if current.Pr != nil {
				openURL(current.Pr.URL)
			} else {
				openURL(site + "/browse/" + current.Key)
			}
		}
	case k == "ctrl+r":
		return m.reload(m.view)
	case k == "enter":
		if ok {
			m.openForm(current)
		}
	case k == "backspace":
		m.query = dropLast(m.query)
		move(0)
	case text != "":
		m.query += text
		move(0)
	}
	return nil
}

func (m *model) formInput(msg tea.KeyPressMsg) tea.Cmd {
	f := m.form
	if f.busy {
		return nil
	}
	k := msg.String()
	text := msg.Key().Text
	left, right := k == "left", k == "right"
	isPr := f.row.Pr != nil
	switch {
	case k == "esc":
		m.form = nil
	case k == "ctrl+c":
		return tea.Quit
	case k == "enter":
		return m.submit()
	// A review has no options or repo to pick: its fields are agent (0) and context (3).
	case k == "tab" || (k == "down" && f.field != 2):
		if isPr {
			f.field = 3 - f.field
		} else {
			f.field = (f.field + 1) % 4
		}
	case k == "shift+tab" || (k == "up" && f.field != 2):
		if isPr {
			f.field = 3 - f.field
		} else {
			f.field = (f.field + 3) % 4
		}
	case f.field == 0 && (left || right):
		if left {
			f.agent = (f.agent + len(agents) - 1) % len(agents)
		} else {
			f.agent = (f.agent + 1) % len(agents)
		}
	case f.field == 1 && (k == "space" || left || right):
		f.worktree = !f.worktree
	case f.field == 2:
		switch {
		case left || k == "up":
			f.repo = max(0, f.repo-1)
		case right || k == "down":
			f.repo++
		case k == "backspace":
			f.repoQuery, f.repo = dropLast(f.repoQuery), 0
		case text != "":
			f.repoQuery, f.repo = f.repoQuery+text, 0
		}
	case f.field == 3:
		if k == "backspace" {
			f.context = dropLast(f.context)
		} else {
			f.context += text
		}
	}
	return nil
}

func ptrs(ss []string) []*string {
	out := make([]*string, len(ss))
	for n := range ss {
		out[n] = &ss[n]
	}
	return out
}

func dropLast(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

func wrap(s string, width int) []string {
	return strings.Split(ansi.Wrap(s, max(1, width), ""), "\n")
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
		who = cmp(*m.assignee, "unassigned")
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
	if m.width == 0 {
		return v
	}
	const pad = 1
	inner := m.width - pad*2
	var body []string
	if m.form != nil {
		body = m.renderForm(inner, m.height-1)
	} else {
		body = m.renderBoard(inner, m.height-1)
	}
	lines := make([]string, 0, len(body)+1)
	for _, l := range body {
		lines = append(lines, " "+fit(l, inner))
	}
	status := ""
	if m.status != "" {
		status = fg(m.statusColor, m.status)
	}
	lines = append(lines, " "+fit(status, inner))
	v.SetContent(strings.Join(lines, "\n"))
	return v
}

func (m *model) renderBoard(width, h int) []string {
	items := m.visible()
	current, hasCurrent := m.current()

	listRows := max(4, (h-4)*45/100)
	detailRows := h - listRows - 4
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+listRows {
		m.top = m.cursor - listRows + 1
	}
	m.top = max(0, min(m.top, max(0, len(items)-listRows)))

	isPrs := m.view == prs
	out := []string{m.header(fmt.Sprintf("%d/%d", len(items), len(m.rows())))}
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
		whoW = max(whoW, ansi.StringWidth(cmp(i.Assignee, "unassigned")))
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
		} else if i.Assignee == m.me || (i.Pr != nil && i.Pr.Mine) {
			whoColor = cMauve
		}
		line := " "
		if sel {
			line = fg(cMauve, "▌")
		}
		if m.state.Dispatched[i.Key] != "" {
			line += fg(cGreen, "◆")
		} else {
			line += " "
		}
		keyColor, summary := cOverlay, fg(cSubtext, fit(i.Summary, sumW))
		if sel {
			keyColor, summary = cText, bold(fg(cText, fit(i.Summary, sumW)))
		}
		line += strings.Join([]string{
			fg(keyColor, fit(i.Key, keyW)),
			fg(statusColor, fit(i.Status, statusW)),
			fg(whoColor, fit(cmp(i.Assignee, "unassigned"), whoW)),
			summary,
		}, "  ")
		out = append(out, line)
	}

	out = append(out, fg(cSurface, strings.Repeat("─", width)))
	var detail []string
	if hasCurrent {
		if lines := m.details[current.Key]; lines != nil {
			detail = lines
		} else {
			detail = []string{fmt.Sprintf("# %s: %s", current.Key, current.Summary), "", fg(cOverlay, "Loading…")}
		}
	}
	var wrapped []string
	for _, l := range detail {
		if l == "" {
			wrapped = append(wrapped, "")
		} else {
			wrapped = append(wrapped, wrap(markdownLine(l), width)...)
		}
	}
	m.detailScroll = max(0, min(m.detailScroll, len(wrapped)-detailRows))
	for n := 0; n < detailRows; n++ {
		if at := m.detailScroll + n; at < len(wrapped) {
			out = append(out, wrapped[at])
		} else {
			out = append(out, "")
		}
	}

	if isPrs {
		out = append(out, fg(cOverlay, "↑↓ pick · enter review · tab tickets · ^o requested/mine · ^a author · ^u/^d scroll · ^b browser · ^r refresh · esc quit"))
	} else {
		out = append(out, fg(cOverlay, "↑↓ pick · enter dispatch · tab PRs · ^o mine · ^a assignee · ^u/^d scroll · ^b browser · ^r refresh · esc quit"))
	}
	return out
}

func (m *model) renderForm(width, h int) []string {
	f := m.form
	isPr := f.row.Pr != nil
	verb := "dispatch"
	if isPr {
		verb = "review"
	}
	out := []string{m.header(verb + " " + f.row.Key)}
	out = append(out, fg(cMauve, f.row.Key)+" "+bold(`"`+f.row.Summary+`"`), fg(cMauve, strings.Repeat("─", width)))
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

	out = append(out, label(0, bold("Agent")))
	for n, a := range agents {
		name := a.Label
		if n == f.agent {
			name = bold(name)
		}
		out = append(out, "    "+radio(n == f.agent)+" "+fit(name, 16)+fg(cOverlay, a.Note))
	}
	if isPr {
		p := f.row.Pr
		out = append(out, "", "  "+bold("Checkout"),
			"    "+fg(cSubtext, tilde(fmt.Sprintf("%s-pr-%d", p.Checkout, p.Number)))+" "+fg(cOverlay, "(worktree at the PR head)"))
	} else {
		out = append(out, "", label(1, bold("Options")))
		box := "[ ]"
		if f.worktree {
			box = fg(cGreen, "[x]")
		}
		out = append(out, "    "+box+" git worktree "+fg(cOverlay, "(isolated checkout, parallel-safe)"), "")
		choices := m.repoChoices()
		f.repo = max(0, min(f.repo, len(choices)-1))
		filter := fg(cOverlay, "type to filter")
		if f.repoQuery != "" {
			filter = fg(cMauve, "/"+f.repoQuery)
		}
		out = append(out, label(2, bold("Repo")+" "+filter))
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
	out = append(out, "", label(3, bold("Extra context")+" "+fg(cOverlay, "(optional)")))
	if f.context != "" || f.field == 3 {
		cursor := ""
		if f.field == 3 {
			cursor = "█"
		}
		for _, l := range wrap(f.context+cursor, width-4) {
			out = append(out, "    "+l)
		}
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
		fieldKeys := []string{"←→ agent", "space toggle", "←→ repo", "type"}[f.field]
		out = append(out, fg(cOverlay, fmt.Sprintf("tab/↑↓ field · %s · enter %s · esc back", fieldKeys, verb)))
	}
	return out
}

func run() error {
	argv := os.Args[1:]
	if len(argv) > 0 && (argv[0] == "--help" || argv[0] == "-h") {
		fmt.Println("usage: foreman [PROJECT | JQL]\n       foreman --logout   forget the Atlassian sign-in")
		return nil
	}
	if len(argv) > 0 && argv[0] == "--logout" {
		signOut()
		return nil
	}

	jql, title := parseArgs(argv)
	// The board draws at once; Jira connects on the first call, signing in through the browser if needed.
	jira := &Jira{log: func(l string) { prog.Send(statusMsg{strings.ReplaceAll(l, "\n", " "), cYellow}) }}
	defer jira.close()

	m := &model{
		jira:        jira,
		jql:         jql,
		title:       title,
		prFilter:    "all",
		inFlight:    map[viewKind]bool{},
		statusColor: cOverlay,
		details:     map[string][]string{},
		state:       loadState(),
		repos:       listRepos(),
	}
	prog = tea.NewProgram(m)
	_, err := prog.Run()
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
