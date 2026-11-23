package main

// The queue's screen. Size follows urgency: what waits on me is a card that says what it asks or
// found, what moves without me a line or two, and what is at rest a quieter line. The row under
// the cursor opens in the reading pane.

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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
	return screen(q, q.width, q.height)
}

// body is the queue's list, or the view it has open in its place.
func (q *queueModel) body(width, h int) []string {
	switch {
	case q.help:
		return indent(renderHelp("queue", queueHelp, width-2, h, &q.helpScroll), 1)
	case q.review != nil:
		return indent(q.renderReview(width-2, h), 1)
	case q.tab == todayTab:
		return indent(q.renderToday(width-2, h), 1)
	case q.tab == statsTab:
		return indent(q.renderStats(width-2, h), 1)
	}
	return q.render(width, h)
}

func (q *queueModel) statusLine() (string, Color, time.Time) {
	return q.status, q.statusColor, q.statusAt
}

// reader is the reading pane of the item under the cursor: its brief, or its whole text.
func (q *queueModel) reader(width, h int) []string {
	it, ok := q.current()
	if q.reading == 0 || !ok || q.help || q.review != nil || q.tab != listTab || q.feedback != nil {
		return nil
	}
	tw := paneText(width)
	if q.reading == readBrief {
		return panel(q.itemBrief(it, tw, h-2), width, h)
	}
	kick, _ := q.itemKicker(it)
	return panel(readAllOf(kick, renderMarkdown(q.detailLines(it), tw), &q.scroll, tw, h-2), width, h)
}

// detailLines are the item's whole text: its report, evidence and screen, or a PR's
// description, with what its session listens on.
func (q *queueModel) detailLines(it queueItem) []string {
	lines := q.details[detailKey(it)]
	if lines == nil {
		lines = []string{"# " + it.Key + ": " + it.Title, "", fg(cOverlay, "Loading…")}
	}
	// Not part of the detail that is kept: the ports change while a report stays as it is.
	// Under a prompt or a question, which the detail starts with, else over the rest.
	switch {
	case len(it.Ports) == 0:
	case it.Agent == "blocked" || it.Question != nil:
		lines = append(slices.Clone(lines), "", portsLine(it.Ports))
	default:
		lines = append([]string{portsLine(it.Ports), ""}, lines...)
	}
	return lines
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

var groupNames = map[group]string{needsYou: "needs you", toReview: "review", working: "working", waiting: "waiting", idle: "idle", closed: "closed"}

// itemLabel is the item's coloured mark and its state.
func itemLabel(it queueItem) (string, string) {
	if it.Kind == prItem {
		what := cmp.Or(it.News, it.Wait, it.Pr.Status)
		switch it.Group {
		case needsYou:
			return fg(cRed, "✗"), fg(cRed, what)
		case toReview:
			return fg(cGreen, "✓"), fg(cGreen, what)
		}
		return fg(cOverlay, "○"), fg(cOverlay, what)
	}
	what := "done"
	if it.review() {
		what = "reviewed"
	}
	if it.Report == "" {
		what = "no report"
	}
	// Its work is in a PR of mine, which the queue lists: the row says which.
	if _, n, ok := strings.Cut(it.InPr, "#"); ok {
		what = "in #" + n
	}
	if it.Orchestrator {
		what = "waiting for you"
	}
	if it.Watcher {
		what = "between passes"
		if it.Polled {
			what = "waits for a PR to change"
		}
		if it.At != "" {
			what = "waits for " + it.At
			if it.Polled {
				what = "waits for a PR to change, or " + it.At
			}
		}
		if it.Unwoken {
			what = "nothing wakes it: foreman watch isn't running"
		}
		if it.Agent == "" {
			what = "not running, enter starts it"
		}
	}
	switch {
	case it.Question != nil && it.Left && it.Agent == "":
		return fg(cRed, "●"), fg(cRed, "left a question") + fg(cOverlay, " (closed)")
	case it.Question != nil && it.Left:
		return fg(cRed, "●"), fg(cRed, "left a question")
	case it.Question != nil:
		return fg(cRed, "●"), fg(cRed, "asks you")
	case len(it.Decisions) > 0 && it.Group == toReview:
		return fg(cGreen, "✓"), fg(cGreen, fmt.Sprintf("went on, %d to confirm", len(it.Decisions)))
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
	case it.Unwoken:
		return fg(cRed, "○"), fg(cRed, what)
	case it.Group == idle:
		return fg(cGreen, "○"), fg(cOverlay, what)
	}
	return fg(cOverlay, "◇"), fg(cOverlay, what)
}

// portsMark is the row's note of what the agent's session listens on: its lowest port, and how
// many more there are.
func portsMark(ports []rexPort) string {
	if len(ports) == 0 {
		return ""
	}
	mark := ":" + strconv.Itoa(ports[0].Port)
	if len(ports) > 1 {
		mark += fmt.Sprintf(" +%d", len(ports)-1)
	}
	return mark
}

// portsLine is the detail's line about those ports: where each is opened and what holds it.
func portsLine(ports []rexPort) string {
	var urls []string
	for _, p := range ports {
		u := p.url()
		if p.Proc != "" {
			u += " (" + p.Proc + ")"
		}
		urls = append(urls, u)
	}
	return "**Listening:** " + strings.Join(urls, " · ") + ", `p` opens it"
}

// shortKey is the item's key as a row has room for it: #1434, task 18, fix #12.
func shortKey(it queueItem) string {
	key := it.Key
	n := strings.Index(key, "#")
	switch {
	case it.Kind == prItem && n >= 0:
		return key[n:]
	case isTask(key):
		return "task " + key[strings.LastIndex(key, "-")+1:]
	case isFix(key) && n >= 0:
		return "fix " + key[n:]
	case it.review() && n >= 0:
		return "review " + key[n:]
	}
	return key
}

// itemTitle is the title a row shows: without its tags, or for a PR under the one it is stacked
// on, what its title adds to that one's.
func itemTitle(it queueItem) string {
	switch {
	case it.Under != "":
		// What it adds keeps its kind, docs: or fix:, and drops its tags.
		rest := it.Title[sharedLead(it.Under, it.Title):]
		kind := conventionalRe.FindString(rest)
		return "↳ " + kind + strings.TrimLeft(tagRe.ReplaceAllString(strings.TrimSpace(rest[len(kind):]), ""), " ")
	case it.Watcher || it.Orchestrator:
		return it.Title
	}
	return displayTitle(it.Title)
}

// doc is the item's report read for a glance, once for what the item shows.
func (q *queueModel) doc(it queueItem) doc {
	if it.Kind != agentItem || it.Report == "" {
		return doc{}
	}
	key := detailKey(it)
	if d, ok := q.docs[key]; ok {
		return d
	}
	b, _ := os.ReadFile(it.Report)
	d := parseDoc(strings.Split(string(b), "\n"))
	if q.docs == nil {
		q.docs = map[string]doc{}
	}
	q.docs[key] = d
	return d
}

// render is the queue's list, h lines: a card for each item that waits on me, then the agents
// at work, then what waits on others and what is at rest.
func (q *queueModel) render(width, h int) []string {
	var box []string
	if f := q.feedback; f != nil {
		verb := "Reply to "
		if f.answer {
			verb = "Answer "
		}
		box = append(box, fg(cSurface, strings.Repeat("─", width)), " "+bold(verb+f.key))
		for _, l := range wrap(cursorText(f.text, f.back), width-4) {
			box = append(box, "   "+l)
		}
	}
	rows := max(1, h-len(box))
	lines, from, to := q.listLines(width)
	q.top = scrollSpan(q.top, max(0, from-1), to, rows, len(lines))
	// The cards come first: while the cursor's fits from the top, the top stays.
	if to < rows {
		q.top = 0
	}
	shown := window(lines, &q.top, rows)
	if q.top+rows < len(lines) {
		shown[rows-1] = " " + fg(cOverlay, "↓ more below")
	}
	return append(shown, box...)
}

// listLines are the list's lines, and the first and last of the item under the cursor.
func (q *queueModel) listLines(width int) (lines []string, from, to int) {
	heading := func(name string, c Color) {
		lines = append(lines, "", " "+fg(c, name))
	}
	count := ""
	if n := q.needYou(); n > 0 {
		count = fmt.Sprint(n)
	}
	if q.prsLoading {
		count = strings.TrimSpace("loading PRs…  " + count)
	}
	lines = append(lines, lr(" "+bold("Your move"), fg(cOverlay, count)+" ", width))
	if q.needYou() == 0 {
		lines = append(lines, "", "   "+fg(cOverlay, "Nothing waits on you."))
	}
	if len(q.items) == 0 {
		lines = append(lines, "", "   "+fg(cOverlay, "no agents dispatched"))
	}
	// What each row at rest says lines up in a column.
	whatW := 0
	for _, it := range q.items {
		if _, what := itemLabel(it); !it.pending() && it.Group != working {
			whatW = max(whatW, textWidth(what))
		}
	}
	whatW = min(whatW, max(12, width*2/5))
	extraW := 0
	for _, it := range q.items {
		if !it.pending() && it.Group != working {
			extraW = max(extraW, textWidth(portsMark(it.Ports)))
		}
	}
	prev := group(-1)
	for n, it := range q.items {
		sel := n == q.cursor && !q.unfocused
		start := len(lines)
		switch {
		case it.pending():
			lines = append(lines, "")
			lines = append(lines, q.card(it, sel, width)...)
		case it.Group == working:
			if prev != working {
				heading("Agents at work", cSubtext)
			}
			lines = append(lines, q.workingRow(it, sel, width)...)
		default:
			rest := it.Group == idle || it.Group == closed
			switch {
			case it.Group == waiting && prev != waiting:
				heading("Waiting on others", cSubtext)
			case rest && prev != idle && prev != closed:
				heading("At rest", cOverlay)
			}
			lines = append(lines, q.restRow(it, sel, width, whatW, extraW)...)
		}
		if n == q.cursor {
			from, to = start, len(lines)-1
		}
		prev = it.Group
	}
	return lines, from, to
}

// card is an item that waits on me, with what it asks or what it found.
func (q *queueModel) card(it queueItem, sel bool, width int) []string {
	cw := paneText(width)
	mark, what := itemLabel(it)
	when := it.When
	if asked, _, ok := it.pendingQuestion(); ok && when.IsZero() {
		when = asked.At
	}
	body := []string{lr(mark+" "+bold(shortKey(it))+" "+what, fg(cOverlay, age(when)), cw)}
	for _, l := range wrapN(itemTitle(it), cw, 2) {
		body = append(body, fg(cSubtext, l))
	}
	if more := q.cardBody(it, cw); len(more) > 0 {
		body = append(append(body, ""), more...)
	}
	bg, bar := bgPanel, " "
	if sel {
		bg, bar = bgSelected, fg(cMauve, "▌")
	}
	out := []string{bar + paint(bg, "", width-1)}
	for _, l := range body {
		out = append(out, bar+paint(bg, "  "+l, width-1))
	}
	return append(out, bar+paint(bg, "", width-1))
}

// cardBody is what a card says under its title: the question and its choices, what the pane
// asks, or the report's headline and a line of its facts.
func (q *queueModel) cardBody(it queueItem, width int) []string {
	if asked, decision, ok := it.pendingQuestion(); ok {
		out := wrapN(asked.Text, width, 6)
		if rows := choiceRows(asked, decision, width, 2); len(rows) > 0 {
			out = append(append(out, ""), rows...)
		}
		return out
	}
	if it.Kind == prItem {
		return factsLine(prFacts(it), width)
	}
	var out []string
	if it.Agent == "blocked" {
		asks := promptOf(q.details[detailKey(it)])
		for _, l := range asks[max(0, len(asks)-6):] {
			out = append(out, fg(cSubtext, cut(l, width)))
		}
		if len(asks) == 0 {
			out = append(out, fg(cSubtext, cmp.Or(it.Doing, "It "+waitsFor(it.Waits))))
		}
	}
	d := q.doc(it)
	switch {
	case d.head != "":
		out = append(out, wrapN(inline(d.head), width, 3)...)
	case it.Report == "" && it.Agent != "blocked":
		out = append(out, fg(cOverlay, "It wrote no report."))
	}
	var facts []string
	for _, f := range itemFacts(it) {
		if f.text != "" && f.label != "session" {
			facts = append(facts, fg(cmp.Or(f.tone, cSubtext), f.text))
		}
	}
	if _, points := d.open(); len(points) > 0 {
		facts = append(facts, fg(cYellow, fmt.Sprintf("%d left open", len(points))))
	}
	if len(facts) > 0 {
		out = append(out, "", strings.Join(facts, fg(cSurface, "  ·  ")))
	}
	return out
}

// workingRow is an agent at work: its title, and under it what it says it is doing.
func (q *queueModel) workingRow(it queueItem, sel bool, width int) []string {
	mark, what := itemLabel(it)
	if it.Question == nil && it.Doing != "" {
		what = fg(cYellow, it.Doing)
	}
	second := fg(cOverlay, shortKey(it)+" · ") + what
	if o := oracleMark(it.Oracles); o != "" {
		second += fg(cOverlay, " · ") + o
	}
	if p := portsMark(it.Ports); p != "" {
		second += fg(cOverlay, " · "+p)
	}
	title := itemTitle(it)
	text := fg(cSubtext, title)
	if sel {
		text = bold(fg(cText, title))
	}
	out := []string{row(sel, mark, text, fg(cOverlay, age(it.When)), width), barLine(sel, "  "+second, width)}
	if sel {
		out = append(out, peek(q.rowPeek(it, width-5), 3, width)...)
	}
	return out
}

// restRow is an item that waits on others or rests: one line, quieter for what rests.
func (q *queueModel) restRow(it queueItem, sel bool, width, whatW, portsW int) []string {
	if it.Fold > 0 {
		text := fg(cOverlay, fmt.Sprintf("%d closed", it.Fold))
		if sel {
			text = bold(fg(cText, fmt.Sprintf("%d closed", it.Fold)))
		}
		return []string{row(sel, fg(cOverlay, "▸"), text, fg(cOverlay, "c lists them"), width)}
	}
	mark, what := itemLabel(it)
	tone := cSubtext
	if it.Group != waiting {
		tone = cOverlay
	}
	keyColor, title := cOverlay, fg(tone, itemTitle(it))
	if sel {
		keyColor, title = cText, bold(fg(cText, itemTitle(it)))
	}
	right := ""
	if portsW > 0 {
		right = fg(cOverlay, fit(portsMark(it.Ports), portsW)) + "  "
	}
	right += fit(what, whatW) + fg(cOverlay, fmt.Sprintf(" %4s", age(it.When)))
	out := []string{row(sel, mark, fg(keyColor, fit(shortKey(it), 9))+" "+title, right, width)}
	if sel {
		out = append(out, peek(q.rowPeek(it, width-15), 13, width)...)
	}
	return out
}

// barLine is a row's further line, marked as the cursor's when it is.
func barLine(sel bool, text string, width int) string {
	if sel {
		return selected(fg(cMauve, "▌")+text, width)
	}
	return " " + text
}

// rowPeek is what the row under the cursor shows under itself while no reading pane does: the
// end of a working agent's screen, a PR's first sentence and facts, a report's headline.
func (q *queueModel) rowPeek(it queueItem, width int) []string {
	detail := q.details[detailKey(it)]
	switch {
	case it.Fold > 0 || q.reading > 0:
		return nil
	case it.Group == working:
		var out []string
		for _, l := range screenOf(detail, 3) {
			out = append(out, cut(l, width))
		}
		return out
	case it.Kind == prItem:
		var out []string
		if detail != nil {
			if d := parseDoc(descriptionOf(detail)); d.head != "" {
				out = wrapN(d.head, width, 2)
			}
		}
		var facts []string
		for _, f := range prFacts(it) {
			if f.text != "" {
				facts = append(facts, f.label+" "+f.text)
			}
		}
		return append(out, cut(strings.Join(facts, " · "), width))
	}
	if d := q.doc(it); d.head != "" {
		return wrapN(d.head, width, 2)
	}
	return nil
}

// keys are the footer's: at most five for the moment and the row under the cursor, then ?.
func (q *queueModel) keys() string {
	switch {
	case q.help:
		return "↑↓ scroll · any other key back"
	case q.review != nil:
		return q.reviewKeys()
	case q.tab == todayTab:
		return "↑↓ pick · enter go to it, dispatch or open · o open · space scroll · tab stats · ? keys"
	case q.tab == statsTab:
		return "↑↓ pick · ←→ today, 7 or 30 days · g by ticket or repo · tab the queue · ? keys"
	case q.feedback != nil:
		return "type the reply · enter send · esc cancel"
	case q.reading == readAll:
		return "^u ^d scroll · esc back · ↑↓ the next one · ? keys"
	}
	it, ok := q.current()
	if !ok {
		return "^l board · tab today · ^r reload · ? keys"
	}
	keys := q.rowKeys(it)
	if q.reading == readBrief {
		if n := slices.Index(keys, "space read"); n >= 0 {
			keys = slices.Delete(keys, n, n+1)
		}
		keys = append([]string{"space the whole text", "esc back"}, keys...)
	}
	return strings.Join(append(keys[:min(5, len(keys))], "? keys"), " · ")
}

// rowKeys are the keys of the row under the cursor, the likeliest first.
func (q *queueModel) rowKeys(it queueItem) []string {
	asked, decision, asks := it.pendingQuestion()
	pick := ""
	if n := len(asked.Options); n > 0 {
		pick = fmt.Sprintf("1–%d choose", n)
	}
	var keys []string
	switch {
	case it.Fold > 0:
		return []string{"enter list them", "space read", "^l board"}
	case asks && decision:
		keys = nonEmpty("a confirm", pick, "x confirm all", "f another answer", "space read")
	case asks:
		recommended := ""
		if asked.Recommended != "" {
			recommended = "a take the recommended"
		}
		keys = nonEmpty(pick, recommended, "f write an answer", "space read", "enter go to it")
	case pressable(it) && q.seen[detailKey(it)] != "":
		keys = []string{"y press enter", "n press esc", "space read", "enter go to it", "f reply"}
	case it.Kind == prItem:
		keys = []string{"enter go to its agent", "o open the PR", "space read"}
		if it.pending() {
			keys = append(keys, "x seen")
		}
	case it.Group == toReview:
		keys = []string{"space read", "x reviewed", "d diff", "r review it", "enter go to it"}
		if it.Shots+it.Videos > 0 {
			keys[3] = "v evidence"
		}
	case it.Agent == "" && it.Watcher:
		keys = []string{"enter start it", "space read"}
	case it.Agent == "":
		keys = []string{"enter resume", "space read", "K K drop", "c fold"}
	case it.Group == working:
		keys = []string{"enter go to it", "f reply", "space read", "^l board"}
	default:
		keys = []string{"enter go to it", "f reply", "space read", "d diff", "K K close"}
	}
	if len(it.Ports) > 0 {
		keys = slices.Insert(keys, min(2, len(keys)), "p open "+portsMark(it.Ports[:1]))
	}
	return keys
}
