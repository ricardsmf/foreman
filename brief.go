package main

// The reading pane's first view of an item, its brief: what it is, what it asks or found, the
// facts, what it leaves open and what else its report holds. The whole text is a key further.

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"
)

// How far the reading pane is open: the brief, then the whole text.
const (
	readBrief = 1
	readAll   = 2
)

// A section is a part of a report or a description, under its "## " heading.
type section struct {
	name  string
	lines []string
}

// A doc is a report or a description read for a glance: its headline, and its sections.
type doc struct {
	head     string
	sections []section
}

var (
	linkRe   = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	escapeRe = regexp.MustCompile(`\\([\[\]_*#>-])`)
	// openRe names the section where a report keeps what it leaves to me.
	openRe = regexp.MustCompile(`(?i)^(open|open points|open questions|left open|left for you|not covered|to decide|risks?|follow-ups?)\s*(\(.*\)|:.*)?$`)
)

// parseDoc reads markdown for its headline, the first "# " line, and its sections. A text with
// no headline, as a PR's description, has its first sentence of prose stand for one.
func parseDoc(lines []string) doc {
	var d doc
	cur := section{}
	fenced := false
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
		}
		switch {
		case fenced:
			cur.lines = append(cur.lines, l)
		case strings.HasPrefix(l, "# ") && d.head == "" && len(d.sections) == 0 && cur.name == "":
			d.head = plain(l[2:])
		case strings.HasPrefix(l, "## "):
			if cur.name != "" || len(prose(cur.lines)) > 0 {
				d.sections = append(d.sections, trimSection(cur))
			}
			cur = section{name: plain(l[3:])}
		default:
			cur.lines = append(cur.lines, l)
		}
	}
	if cur.name != "" || len(prose(cur.lines)) > 0 {
		d.sections = append(d.sections, trimSection(cur))
	}
	if d.head == "" {
		for _, s := range d.sections {
			if p := prose(s.lines); len(p) > 0 {
				d.head = p[0]
				break
			}
		}
	}
	return d
}

// trimSection drops the blank lines around a section's text, which its length leaves out.
func trimSection(s section) section {
	for len(s.lines) > 0 && strings.TrimSpace(s.lines[0]) == "" {
		s.lines = s.lines[1:]
	}
	for len(s.lines) > 0 && strings.TrimSpace(s.lines[len(s.lines)-1]) == "" {
		s.lines = s.lines[:len(s.lines)-1]
	}
	return s
}

// open is what the report leaves to me: the points of its section for them, and the section's name.
func (d doc) open() (string, []string) {
	for _, s := range d.sections {
		if openRe.MatchString(s.name) {
			return s.name, prose(s.lines)
		}
	}
	return "", nil
}

// outline is the report's other sections, those the brief doesn't show.
func (d doc) outline() []section {
	var out []section
	for _, s := range d.sections {
		if s.name != "" && !openRe.MatchString(s.name) {
			out = append(out, s)
		}
	}
	return out
}

// prose is the paragraphs and list items of lines, each on one line, without their code and
// tables. A nested item goes on with the item above it.
func prose(lines []string) []string {
	var out []string
	fenced, joined := false, false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		item := strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") || orderedRe.MatchString(t)
		switch {
		case strings.HasPrefix(t, "```"):
			fenced, joined = !fenced, false
		case fenced || t == "" || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "#"):
			joined = false
		case item && t != l && len(out) > 0:
			out[len(out)-1] += " " + plain(listIndent.ReplaceAllString(t, ""))
		case item:
			out, joined = append(out, plain(listIndent.ReplaceAllString(t, ""))), true
		case joined:
			out[len(out)-1] += " " + plain(t)
		default:
			out, joined = append(out, plain(t)), true
		}
	}
	return out
}

var orderedRe = regexp.MustCompile(`^\d+[.)]\s`)

// unlink keeps a link's text and drops the backslashes markdown escapes with.
func unlink(l string) string {
	return escapeRe.ReplaceAllString(linkRe.ReplaceAllString(l, "$1"), "$1")
}

// plain drops a line's markdown marks.
func plain(l string) string {
	l = boldRe.ReplaceAllString(unlink(l), "$1")
	l = strings.TrimSpace(codeRe.ReplaceAllString(l, "$1"))
	if len(l) > 2 && strings.HasPrefix(l, "_") && strings.HasSuffix(l, "_") {
		l = l[1 : len(l)-1]
	}
	return l
}

// descriptionOf is a ticket's or a PR's text without the lines its detail starts with: the
// heading, the link and the facts, up to the second blank line after the heading.
func descriptionOf(lines []string) []string {
	start := -1
	for n, l := range lines {
		if strings.HasPrefix(l, "# ") {
			start = n
			break
		}
	}
	if start < 0 {
		return lines
	}
	blanks := 0
	for n := start; n < len(lines); n++ {
		if lines[n] == "" {
			if blanks++; blanks == 2 {
				return lines[n+1:]
			}
		}
	}
	return nil
}

// A fact is a line of the brief's small print: a label and what it is, in tone when it is a state.
type fact struct {
	label, text string
	tone        Color
}

// factsLine sets the facts in a row, wrapped to width.
func factsLine(facts []fact, width int) []string {
	var parts []string
	for _, f := range facts {
		if f.text == "" {
			continue
		}
		parts = append(parts, fg(cOverlay, f.label+" ")+fg(cmp.Or(f.tone, cSubtext), f.text))
	}
	if len(parts) == 0 {
		return nil
	}
	return wrap(strings.Join(parts, fg(cSurface, "  ·  ")), width)
}

// cutTo keeps the first h lines, the last of them an ellipsis when there were more.
func cutTo(lines []string, h int) []string {
	if len(lines) <= h {
		return lines
	}
	return append(lines[:max(0, h-1)], fg(cOverlay, "…"))
}

// readAllOf is a whole text in the reading pane, h lines from *scroll: what it is, and where in
// it the pane is when it is longer than the pane.
func readAllOf(kicker string, text []string, scroll *int, width, h int) []string {
	rows := max(1, h-2)
	shown := window(text, scroll, rows)
	pos := ""
	if len(text) > rows {
		pos = fmt.Sprintf("%d–%d of %d", *scroll+1, min(len(text), *scroll+rows), len(text))
	}
	return append([]string{lr(fg(cOverlay, kicker), fg(cOverlay, pos), width), ""}, shown...)
}

// outlineLines name the sections the brief left out, each with its length.
func outlineLines(d doc, label string, width int) []string {
	secs := d.outline()
	if len(secs) == 0 {
		return nil
	}
	out := []string{"", fg(cOverlay, label)}
	for _, s := range secs {
		out = append(out, lr("  "+fg(cSubtext, s.name), fg(cOverlay, plural(len(s.lines), "line")), min(width, 56)))
	}
	return out
}

// bullets are the points of a list, each wrapped under its mark, at most n of them.
func bullets(points []string, n, width int) []string {
	var out []string
	for _, p := range points[:min(n, len(points))] {
		for i, l := range wrapN(inline(p), width-2, 3) {
			if i == 0 {
				out = append(out, fg(cOverlay, "· ")+l)
			} else {
				out = append(out, "  "+l)
			}
		}
	}
	if len(points) > n {
		out = append(out, fg(cOverlay, fmt.Sprintf("  and %d more", len(points)-n)))
	}
	return out
}

// choiceRows are a question's choices, numbered for the digit that picks each, the one the
// agent recommends or went on with marked.
func choiceRows(q Question, decision bool, width, lines int) []string {
	mark := "recommended"
	if decision {
		mark = "went on with this"
	}
	var out []string
	for n, o := range q.Options {
		if o == q.Recommended {
			o += fg(cGreen, "  "+mark)
		}
		for i, l := range wrapN(o, width-4, lines) {
			lead := "    "
			if i == 0 {
				lead = " " + fg(cMauve, fmt.Sprint(n+1)) + "  "
			}
			out = append(out, lead+l)
		}
	}
	if len(q.Options) == 0 && q.Recommended != "" {
		out = append(out, " "+fg(cMauve, "a")+"  "+q.Recommended+fg(cGreen, "  "+mark))
	}
	return out
}

// itemKicker is the brief's first line: what the item is, and how old.
func (q *queueModel) itemKicker(it queueItem) (left, right string) {
	switch {
	case it.Fold > 0:
		return "Closed agents", ""
	case it.Kind == prItem:
		left = it.Key
		if it.Pr.Base != "" {
			left += " · into " + it.Pr.Base
		}
		return left, "open " + age(it.When)
	}
	parts := []string{it.Key}
	if it.Repo != "" && !strings.HasPrefix(strings.TrimPrefix(it.Key, "task:"), it.Repo) {
		parts = append(parts, it.Repo)
	}
	if a := q.state.Dispatched[it.Key].Agent; a != "" {
		parts = append(parts, a)
	}
	return strings.Join(parts, " · "), age(it.When)
}

// itemFacts are an agent's small print: its PR, oracles, evidence, ports and session.
func itemFacts(it queueItem) []fact {
	var out []fact
	if _, n, ok := strings.Cut(it.InPr, "#"); ok {
		out = append(out, fact{"pull", "#" + n, ""})
	}
	if len(it.Oracles) > 0 {
		tone := cGreen
		if greens, unseen := oracleTally(it.Oracles); greens < len(it.Oracles) || unseen > 0 {
			tone = cYellow
		}
		out = append(out, fact{"oracles", oracleCount(it.Oracles), tone})
	}
	out = append(out, fact{"evidence", evidenceMark(it), ""}, fact{"listening", portsMark(it.Ports), ""})
	if it.Agent == "" && it.Kind == agentItem {
		out = append(out, fact{"session", "closed", ""})
	}
	return out
}

// prFacts are a PR's small print: what someone did on it, its checks, its reviews, and where it goes.
func prFacts(it queueItem) []fact {
	p := it.Pr
	checks := p.CI
	if p.CI == "failed" {
		checks = "failed: " + listed(p.Failing, 2)
	}
	tone := Color("")
	switch p.CI {
	case "failed":
		tone = cRed
	case "running":
		tone = cYellow
	}
	review := p.Review
	if review == "" && len(p.Reviewers) > 0 {
		review = "asked of " + listed(p.Reviewers, 2)
	}
	return []fact{{"new", it.News, ""}, {"checks", checks, tone}, {"review", review, ""}, {"stacked on", it.On, ""}}
}

// evidenceMark counts the item's screenshots and videos.
func evidenceMark(it queueItem) string {
	var ev []string
	if it.Shots > 0 {
		ev = append(ev, fmt.Sprintf("▣ %d", it.Shots))
	}
	if it.Videos > 0 {
		ev = append(ev, "▶ video")
	}
	return strings.Join(ev, " ")
}

// promptOf is what a blocked agent's pane asks, from the detail that starts with it.
func promptOf(detail []string) []string {
	if len(detail) == 0 || detail[0] != "# It waits on this" {
		return nil
	}
	var out []string
	in := false
	for _, l := range detail {
		if strings.HasPrefix(l, "```") {
			if in {
				return out
			}
			in = true
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return out
}

// screenOf is the end of an agent's screen, from its detail: the last n lines with text.
func screenOf(detail []string, n int) []string {
	at := -1
	for i, l := range detail {
		if l == "## Its screen" {
			at = i
		}
	}
	if at < 0 {
		return nil
	}
	var out []string
	for _, l := range detail[at+1:] {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out[max(0, len(out)-n):]
}

// itemBrief is the brief of the item under the queue's cursor, h lines width wide.
func (q *queueModel) itemBrief(it queueItem, width, h int) []string {
	kick, right := q.itemKicker(it)
	out := []string{lr(fg(cOverlay, kick), fg(cOverlay, right), width), ""}
	if it.Fold > 0 {
		out = append(out, bold(plural(it.Fold, "agent")+" whose session is closed"), "")
		return cutTo(append(out, renderMarkdown(foldDetail(q.all)[2:], width)...), h)
	}
	out = append(out, wrapN(bold(itemTitle(it)), width, 3)...)
	out = append(out, "")
	detail := q.details[detailKey(it)]
	d := q.doc(it)
	asked, decision, asks := it.pendingQuestion()
	switch {
	case asks:
		label := fg(cRed, "Asks you")
		switch {
		case decision:
			label = fg(cGreen, "Went on without an answer")
		case it.Left:
			label = fg(cRed, "Left a question") + fg(cOverlay, ": an answer is typed into its pane")
			if it.Agent == "" {
				label = fg(cRed, "Left a question") + fg(cOverlay, ": an answer resumes it")
			}
		}
		out = append(out, label, "")
		out = append(out, wrap(asked.Text, width)...)
		if rows := choiceRows(asked, decision, width, 3); len(rows) > 0 {
			out = append(append(out, ""), rows...)
		}
		if n := len(it.Decisions); n > 1 || (n > 0 && !decision) {
			out = append(out, "", fg(cOverlay, plural(n, "more question")+" it went on without, in the whole text"))
		}
		if d.head != "" {
			out = append(append(out, ""), wrapN(inline(d.head), width, 3)...)
			out = append(out, outlineLines(d, "Also in the report", width)...)
		}
	case it.Kind == prItem:
		_, what := itemLabel(it)
		out = append(out, fg(cOverlay, "Waits on  ")+what)
		out = append(out, factsLine(prFacts(it), width)...)
		if detail == nil {
			out = append(out, "", fg(cOverlay, "Loading…"))
			break
		}
		pd := parseDoc(descriptionOf(detail))
		if pd.head != "" {
			out = append(append(out, ""), wrapN(inline(pd.head), width, 6)...)
		}
		out = append(out, outlineLines(pd, "Also in the PR", width)...)
	case it.Agent == "blocked":
		out = append(out, fg(cRed, "It "+waitsFor(it.Waits)))
		if it.Doing != "" {
			out = append(out, fg(cSubtext, it.Doing))
		}
		if asks := promptOf(detail); len(asks) > 0 {
			out = append(out, "")
			for _, l := range asks {
				out = append(out, fg(cSubtext, cut(l, width)))
			}
		}
		out = append(append(out, ""), factsLine(itemFacts(it), width)...)
	case it.Group == working:
		line := fg(cYellow, cmp.Or(it.Doing, "working"))
		if o := oracleMark(it.Oracles); o != "" {
			line += fg(cSurface, "  ·  ") + o
		}
		out = append(out, line, "")
		screen := screenOf(detail, h)
		for _, l := range screen[max(0, len(screen)-(h-len(out))):] {
			out = append(out, fg(cSubtext, cut(l, width)))
		}
	case d.head != "":
		out = append(out, wrapN(inline(d.head), width, 6)...)
		if facts := factsLine(itemFacts(it), width); len(facts) > 0 {
			out = append(append(out, ""), facts...)
		}
		if name, points := d.open(); len(points) > 0 {
			out = append(out, "", bold(name))
			out = append(out, bullets(points, 4, width)...)
		}
		out = append(out, outlineLines(d, "Also in the report", width)...)
	default:
		_, what := itemLabel(it)
		out = append(out, what)
		if facts := factsLine(itemFacts(it), width); len(facts) > 0 {
			out = append(out, facts...)
		}
		if detail != nil {
			out = append(append(out, ""), renderMarkdown(detail, width)...)
		}
	}
	return cutTo(out, h)
}

// rowFacts are a board row's small print: the tags its title starts with, its parent, and its agent.
func (m *model) rowFacts(r Row) []fact {
	var out []fact
	if tags := titleTags(r.Summary); len(tags) > 0 {
		out = append(out, fact{"area", strings.Join(tags, ", "), ""})
	}
	if p := r.Pr; p != nil {
		checks, tone := p.CI, Color("")
		if p.CI == "failed" {
			checks, tone = "failed: "+listed(p.Failing, 2), cRed
		}
		out = append(out, fact{"checks", checks, tone}, fact{"review", p.Review, ""}, fact{"new", p.News, ""})
	}
	key := m.agentKey(r.Key)
	if d := m.state.Dispatched[key]; d.Dir != "" {
		what, tone := "session closed", Color("")
		switch m.agents[d.Session] {
		case "blocked", "error":
			what, tone = "waits on you", cRed
		case "working":
			what, tone = "at work", cYellow
		case "done", "idle", "open":
			what = "idle, session open"
		}
		out = append(out, fact{"agent", what, tone})
	}
	return out
}

// rowBrief is the brief of the board's row under the cursor: what it is, its facts, and its
// description as far as the pane goes.
func (m *model) rowBrief(r Row, width, h int) []string {
	kick, right := r.Key, strings.ToLower(r.Status)
	if i := r.Issue; i != nil {
		kick = strings.Join(nonEmpty(r.Key, i.Type, i.Priority), " · ")
		right = strings.Join(nonEmpty(right, cmp.Or(r.Assignee, "unassigned")), " · ")
	}
	if p := r.Pr; p != nil {
		kick = strings.Join(nonEmpty(r.Key, p.Branch), " · ")
		right = strings.Join(nonEmpty(right, r.Assignee), " · ")
	}
	out := []string{lr(fg(cOverlay, kick), fg(cOverlay, right), width), ""}
	out = append(out, wrapN(bold(displayTitle(r.Summary)), width, 3)...)
	if facts := factsLine(m.rowFacts(r), width); len(facts) > 0 {
		out = append(append(out, ""), facts...)
	}
	out = append(out, "")
	switch detail := m.details[r.Key]; {
	case detail == nil:
		out = append(out, fg(cOverlay, "Loading…"))
	default:
		out = append(out, renderMarkdown(descriptionOf(detail), width)...)
	}
	return cutTo(out, h)
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
