package main

// The queue's two other views, which tab turns to: today, and the stats.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type queueTab int

const (
	listTab queueTab = iota
	todayTab
	statsTab
)

type (
	// queueTicketsMsg carries the board's tickets to the queue, for today's up next.
	queueTicketsMsg struct{ issues []Issue }
	queueStatsMsg   struct{ report statsReport }
)

// statPeriods are the days the stats view counts, which ←→ turn through, and statsRefresh how
// often it counts again while it shows.
var statPeriods = []int{1, 7, 30}

const statsRefresh = time.Minute

// showTab turns the queue to a view, loading what it shows.
func (q *queueModel) showTab(t queueTab) tea.Cmd {
	q.tab, q.scroll = t, 0
	switch t {
	case todayTab:
		q.loadToday()
	case statsTab:
		return q.loadStats()
	}
	return nil
}

func (q *queueModel) loadToday() {
	q.events = readEvents()
	q.today = todayOf(q.state, q.all, q.issues, q.events, time.Now())
}

// loadStats counts the stats off the screen's thread: it reads every conversation of the period.
func (q *queueModel) loadStats() tea.Cmd {
	if q.statsLoading {
		return nil
	}
	q.statsLoading, q.statsRead = true, time.Now()
	s, days, by := q.state, max(1, q.statsDays), cmp.Or(q.statsBy, byTicket)
	return func() tea.Msg { return queueStatsMsg{statsOf(s, readEvents(), days, by, time.Now())} }
}

// A todayPick is a row of the today view: one of the three lists', and which list.
type todayPick struct {
	section int
	row     todayRow
}

var todaySections = []string{"progress", "up next", "done"}

func (q *queueModel) todayRows() []todayPick {
	var out []todayPick
	for n, rows := range [][]todayRow{q.today.InProgress, q.today.UpNext, q.today.Done} {
		for _, r := range rows {
			out = append(out, todayPick{n, r})
		}
	}
	return out
}

// agent is the dispatched agent's item with the key.
func (q *queueModel) agent(key string) (queueItem, bool) {
	n := slices.IndexFunc(q.all, func(it queueItem) bool { return it.Kind == agentItem && it.Key == key })
	if n < 0 {
		return queueItem{}, false
	}
	return q.all[n], true
}

// tabInput takes a key on the today or the stats view. handled is false for the keys every view
// has, which the queue's own input takes; the rest of the queue's keys act on a row of its
// list, which isn't showing.
func (q *queueModel) tabInput(msg tea.KeyPressMsg) (cmd tea.Cmd, handled bool) {
	key := msg.String()
	switch key {
	case "ctrl+c", "q", "ctrl+u", "ctrl+d", "space", "?", "tab":
		return nil, false
	}
	if q.tab == statsTab {
		return q.statsInput(key), true
	}
	return q.todayInput(key), true
}

func (q *queueModel) todayInput(key string) tea.Cmd {
	rows := q.todayRows()
	switch key {
	case "up", "k", "ctrl+p":
		q.todayCursor, q.scroll = q.todayCursor-1, 0
	case "down", "j", "ctrl+n":
		q.todayCursor, q.scroll = q.todayCursor+1, 0
	case "ctrl+r":
		return tea.Batch(pollQueue, q.loadPrs())
	case "enter", "s", "o", "O":
		if q.todayCursor < 0 || q.todayCursor >= len(rows) {
			return nil
		}
		p := rows[q.todayCursor]
		if key == "enter" || key == "s" {
			switch p.section {
			case 0:
				if it, ok := q.agent(p.row.Key); ok {
					return q.open(it)
				}
			case 1:
				return func() tea.Msg { return openFormMsg{p.row.Key} }
			}
		}
		if p.row.URL == "" {
			q.say(p.row.Key+" has no ticket or PR to open", cOverlay)
			return nil
		}
		if key == "O" {
			openURL(p.row.URL)
			return nil
		}
		return q.browse(p.row.URL)
	}
	q.todayCursor = max(0, min(q.todayCursor, len(rows)-1))
	return nil
}

func (q *queueModel) statsInput(key string) tea.Cmd {
	at := max(0, slices.Index(statPeriods, max(1, q.statsDays)))
	switch key {
	case "up", "k", "ctrl+p":
		q.statsCursor, q.scroll = q.statsCursor-1, 0
	case "down", "j", "ctrl+n":
		q.statsCursor, q.scroll = q.statsCursor+1, 0
	case "left", "h":
		q.statsDays, q.statsCursor = statPeriods[max(0, at-1)], 0
		return q.loadStats()
	case "right", "l":
		q.statsDays, q.statsCursor = statPeriods[min(len(statPeriods)-1, at+1)], 0
		return q.loadStats()
	case "g":
		q.statsBy, q.statsCursor = byRepo, 0
		if q.stats.By == byRepo {
			q.statsBy = byTicket
		}
		return q.loadStats()
	case "ctrl+r":
		return q.loadStats()
	}
	q.statsCursor = max(0, min(q.statsCursor, len(q.stats.Rows)-1))
	return nil
}

// tabLayout splits the h lines under head lines of heading between a list of total rows and the
// detail of the one picked, as the queue's own list does.
func tabLayout(h, head, total int, footer []string) (listRows, detailRows int) {
	listRows = max(3, min(total, (h-head-1-len(footer))*50/100))
	return listRows, h - head - listRows - 1 - len(footer)
}

func (q *queueModel) renderToday(width, h int) []string {
	dot := fg(cSurface, " · ")
	t := q.today
	head := bold(fg(cMauve, "today")) + dot + fg(cSubtext, time.Now().Format("Mon 2 Jan")) + dot +
		fg(cYellow, fmt.Sprintf("%d in progress", len(t.InProgress))) + dot +
		fg(cSubtext, fmt.Sprintf("%d up next", len(t.UpNext))) + dot +
		fg(cGreen, fmt.Sprintf("%d done since yesterday", len(t.Done)))
	out := []string{head}
	rows := q.todayRows()
	footer := hints("↑↓ pick · enter go to it, dispatch or open · o open · tab stats · ? all keys", width)
	listRows, detailRows := tabLayout(h, 1, len(rows), footer)
	q.todayCursor = max(0, min(q.todayCursor, len(rows)-1))
	q.todayTop = scrollTo(q.todayTop, q.todayCursor, listRows, len(rows))
	keyW := 8
	for _, r := range rows {
		keyW = max(keyW, len(r.row.Key))
	}
	keyW = min(22, keyW)
	for n := q.todayTop; n < q.todayTop+listRows; n++ {
		if n >= len(rows) {
			empty := ""
			if n == 0 {
				empty = fg(cOverlay, "  nothing in progress, up next or done since yesterday")
			}
			out = append(out, empty)
			continue
		}
		r, sel := rows[n], n == q.todayCursor
		line := " "
		if sel {
			line = fg(cMauve, "▌")
		}
		// The list's name on its first row, and on the top row when the list starts above it.
		name := ""
		if n == q.todayTop || rows[n-1].section != r.section {
			name = todaySections[r.section]
		}
		line += fg(cOverlay, fit(name, 9)) + " "
		mark, what := q.todayLabel(r)
		meta := ansi.Truncate(what, max(16, width/3), "…")
		if r.row.At != nil {
			meta += "  " + fg(cOverlay, age(*r.row.At))
		}
		keyColor, title := cOverlay, fg(cSubtext, r.row.Title)
		if sel {
			keyColor, title = cText, bold(fg(cText, r.row.Title))
		}
		left := line + mark + " " + fg(keyColor, fit(r.row.Key, keyW)) + "  "
		row := left + fit(title, max(0, width-ansi.StringWidth(left)-ansi.StringWidth(meta)-2)) + "  " + meta
		if sel {
			row = selected(row, width)
		}
		out = append(out, row)
	}
	var detail []string
	if len(rows) > 0 {
		detail = renderMarkdown(q.todayDetail(rows[q.todayCursor]), width)
	}
	shown := window(detail, &q.scroll, detailRows)
	out = append(out, divider(width, q.scroll, detailRows, len(detail)))
	return append(append(out, shown...), footer...)
}

// todayLabel is a row's mark and what it says: a dispatch in progress as its row in the queue,
// a ticket by its status, one done by what ended it.
func (q *queueModel) todayLabel(p todayPick) (mark, what string) {
	switch p.section {
	case 1:
		return fg(cOverlay, "○"), fg(cSubtext, strings.ToLower(p.row.Status))
	case 2:
		return fg(cGreen, "✓"), fg(cGreen, p.row.What)
	}
	if it, ok := q.agent(p.row.Key); ok {
		return itemLabel(it)
	}
	return fg(cOverlay, "◇"), fg(cOverlay, p.row.State)
}

// todayDetail is what shows under the row picked: what it is, and what the log has of its dispatch.
func (q *queueModel) todayDetail(p todayPick) []string {
	r := p.row
	out := []string{"# " + r.Key + ": " + r.Title, ""}
	switch p.section {
	case 1:
		return append(out, "**Status:** "+r.Status, "", r.URL, "", "`enter` opens the board's dispatch form on it.")
	case 2:
		out = append(out, "**Done:** "+r.What+", "+r.At.Local().Format("Mon 2 Jan 15:04"), "")
	default:
		out = append(out, "**Now:** "+r.State, "")
	}
	return append(out, timeline(q.events, r.Key, r.Dispatch)...)
}

func (q *queueModel) renderStats(width, h int) []string {
	dot := fg(cSurface, " · ")
	rep := q.stats
	days := max(1, q.statsDays)
	var periods []string
	for _, d := range statPeriods {
		if d == days {
			periods = append(periods, bold(fg(cText, periodName(d))))
		} else {
			periods = append(periods, fg(cOverlay, periodName(d)))
		}
	}
	head := bold(fg(cMauve, "stats")) + dot + strings.Join(periods, "  ") + dot + fg(cSubtext, "by "+cmp.Or(q.statsBy, byTicket))
	if q.statsLoading {
		head += fg(cYellow, "  counting…")
	}
	totals := fg(cOverlay, "nothing counted yet")
	if q.statsLoaded {
		parts := statsHead(rep)[1:]
		parts[0] = bold(fg(cText, parts[0]))
		totals = strings.Join(parts, dot)
	}
	out := []string{head, totals}
	footer := hints("↑↓ pick · ←→ today, 7 or 30 days · g by ticket or repo · tab queue · ? all keys", width)
	listRows, detailRows := tabLayout(h, 2, len(rep.Rows), footer)
	q.statsCursor = max(0, min(q.statsCursor, len(rep.Rows)-1))
	q.statsTop = scrollTo(q.statsTop, q.statsCursor, listRows, len(rep.Rows))
	nameW := 8
	for _, r := range rep.Rows {
		nameW = max(nameW, len(r.Name))
	}
	nameW = min(22, nameW)
	for n := q.statsTop; n < q.statsTop+listRows; n++ {
		if n >= len(rep.Rows) {
			empty := ""
			if n == 0 && q.statsLoaded {
				empty = fg(cOverlay, "  no agent of foreman's worked in these days")
			}
			out = append(out, empty)
			continue
		}
		r, sel := rep.Rows[n], n == q.statsCursor
		line, nameColor, title := " ", cOverlay, fg(cSubtext, r.Title)
		if sel {
			line, nameColor, title = fg(cMauve, "▌"), cText, bold(fg(cText, r.Title))
		}
		left := line + " " + fg(nameColor, fit(r.Name, nameW)) + "  "
		meta := fmt.Sprintf("%8s  %6s  %7s", money(r.Cost), tokenCount(r.tokens()), workTime(r.Seconds))
		row := left + fit(title, max(0, width-ansi.StringWidth(left)-ansi.StringWidth(meta)-2)) + "  " + fg(cSubtext, meta)
		if sel {
			row = selected(row, width)
		}
		out = append(out, row)
	}
	var detail []string
	if len(rep.Rows) > 0 {
		detail = renderMarkdown(statDetail(rep.Rows[q.statsCursor]), width)
	}
	shown := window(detail, &q.scroll, detailRows)
	out = append(out, divider(width, q.scroll, detailRows, len(detail)))
	return append(append(out, shown...), footer...)
}

// statDetail is a row's tokens by kind, under what they cost and the time its agents worked.
func statDetail(r statRow) []string {
	head := "# " + r.Name
	if r.Title != "" {
		head += ": " + r.Title
	}
	out := []string{head, "",
		"**Cost:** " + money(r.Cost) + " at API prices",
		"**Tokens:** " + tokenCount(r.tokens()) + ": " + tokenCount(r.In) + " in · " + tokenCount(r.Out) + " out · " +
			tokenCount(r.CacheRead) + " cache read · " + tokenCount(r.CacheWrite) + " cache write",
		"**Agent time:** " + workTime(r.Seconds)}
	if r.Repo != "" {
		out = append(out, "**Repo:** "+r.Repo)
	}
	if r.Unpriced > 0 {
		out = append(out, "", tokenCount(r.Unpriced)+" of the tokens are on models foreman has no price for; the cost leaves them out.")
	}
	return out
}
