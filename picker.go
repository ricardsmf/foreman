package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The picker changes the selected ticket's status (^w) or assignee (^y). It takes the detail
// pane's place under the list, so the row it changes stays in view.

type pickKind int

const (
	pickStatus pickKind = iota
	pickAssignee
)

type pickItem struct {
	id string // transition ID, or account ID ("" unassigns)
	// What the row shows once it is picked: the status the transition leads to and its category,
	// or the assignee's name.
	value, category string
	note            string // shown after the value
	search          string // what typing matches, besides the value
	me              bool
}

type picker struct {
	kind  pickKind
	row   Row
	query string
	// items are what Jira returned: the ticket's transitions, or the users matching search.
	items   []pickItem
	search  string
	cursor  int
	loading bool
	busy    bool
	seq     int
}

type (
	pickLoadedMsg struct {
		key   string
		kind  pickKind
		seq   int
		items []pickItem
		me    *JiraUser
		err   error
	}
	pickSearchMsg struct{ seq int }
	// ticketChangedMsg is a write's outcome, with the ticket as Jira now has it.
	ticketChangedMsg struct {
		issue Issue
		done  string
		err   error
	}
)

// openPicker opens the status or assignee picker on the ticket under the cursor.
func (m *model) openPicker(kind pickKind) tea.Cmd {
	row, ok := m.current()
	if !ok {
		return nil
	}
	if row.Issue == nil {
		m.say("Status and assignee are changed on tickets, not PRs", cOverlay)
		return nil
	}
	m.picker = &picker{kind: kind, row: row, loading: true}
	return m.loadPicks("")
}

// loadPicks fetches the transitions, or the assignable users matching query along with me.
func (m *model) loadPicks(query string) tea.Cmd {
	p := m.picker
	p.loading = true
	jira, key, kind, seq, me := m.jira, p.row.Key, p.kind, p.seq, m.me
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
		defer cancel()
		msg := pickLoadedMsg{key: key, kind: kind, seq: seq}
		if kind == pickStatus {
			ts, err := jira.transitions(ctx, key)
			msg.items, msg.err = transitionItems(ts), err
			return msg
		}
		if me == nil {
			if u, err := jira.currentUser(ctx); err == nil {
				me = &u
				msg.me = me
			}
		}
		users, err := jira.assignableUsers(ctx, key, query)
		msg.items, msg.err = userItems(users, me), err
		return msg
	}
}

func transitionItems(ts []Transition) []pickItem {
	var out []pickItem
	for _, t := range ts {
		it := pickItem{
			id:       t.ID,
			value:    cmp.Or(t.To.Name, t.Name),
			category: boardCategory(t.To.Name, categoryNames[t.To.StatusCategory.Key]),
			search:   t.Name,
		}
		// A transition is named apart from where it leads only sometimes; say so when it is.
		if !strings.EqualFold(t.Name, it.value) {
			it.note = "via " + t.Name
		}
		out = append(out, it)
	}
	return out
}

// userItems are the users Jira matched, less me: I am always offered first.
func userItems(users []JiraUser, me *JiraUser) []pickItem {
	var out []pickItem
	for _, u := range users {
		if me != nil && u.AccountID == me.AccountID {
			continue
		}
		out = append(out, pickItem{id: u.AccountID, value: u.DisplayName, note: u.Email, search: u.Email})
	}
	return out
}

// choices are the picker's options matching the query. Assignees start with me and unassigned.
func (m *model) choices() []pickItem {
	p := m.picker
	items := p.items
	if p.kind == pickAssignee {
		fixed := []pickItem{{value: "", note: "unassigned", search: "unassigned none nobody"}}
		if m.me != nil {
			fixed = append([]pickItem{{id: m.me.AccountID, value: m.me.DisplayName, note: "me", search: "me " + m.me.Email, me: true}}, fixed...)
		}
		items = append(fixed, items...)
	}
	words := strings.Fields(strings.ToLower(p.query))
	var out []pickItem
	for _, it := range items {
		hay := strings.ToLower(it.value + " " + it.note + " " + it.search)
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(hay, w) }) {
			out = append(out, it)
		}
	}
	return out
}

func (m *model) pickerInput(msg tea.KeyPressMsg) tea.Cmd {
	p := m.picker
	k := msg.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	// The write is on its way; its outcome closes the picker or reports why it failed.
	if p.busy {
		return nil
	}
	text := msg.Key().Text
	queryWas := p.query
	switch {
	case k == "esc":
		m.picker = nil
		return nil
	case k == "up" || k == "ctrl+p":
		p.cursor = max(0, p.cursor-1)
	case k == "down" || k == "ctrl+n":
		p.cursor++
	case k == "pgup":
		p.cursor = max(0, p.cursor-10)
	case k == "pgdown":
		p.cursor += 10
	case k == "enter":
		items := m.choices()
		if len(items) == 0 {
			return nil
		}
		return m.applyPick(items[min(p.cursor, len(items)-1)])
	case k == "backspace":
		p.query = dropLast(p.query)
	case text != "":
		p.query += text
	}
	if p.query != queryWas {
		return m.queryChanged()
	}
	return nil
}

// queryChanged puts the cursor back on the first match and, for assignees, asks Jira for the
// users matching the new query once typing pauses.
func (m *model) queryChanged() tea.Cmd {
	p := m.picker
	p.cursor = 0
	if p.kind != pickAssignee {
		return nil
	}
	p.seq++
	seq := p.seq
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return pickSearchMsg{seq} })
}

// applyPick writes the choice to Jira, then reads the ticket back: a workflow can land somewhere
// other than where its transition says, and the read gives the row's Mine too.
func (m *model) applyPick(it pickItem) tea.Cmd {
	p := m.picker
	p.busy = true
	jira, kind, issue := m.jira, p.kind, *p.row.Issue
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
		defer cancel()
		var err error
		var done string
		if kind == pickStatus {
			err = jira.transition(ctx, issue.Key, it.id)
			issue.Status, issue.Category = it.value, it.category
			done = fmt.Sprintf("✓ %s is %s", issue.Key, it.value)
		} else {
			err = jira.assign(ctx, issue.Key, it.id)
			issue.Assignee, issue.Mine = it.value, it.me
			done = fmt.Sprintf("✓ %s is assigned to %s", issue.Key, cmp.Or(it.value, "nobody"))
		}
		if err != nil {
			what := "status"
			if kind == pickAssignee {
				what = "assignee"
			}
			return ticketChangedMsg{issue: issue, err: fmt.Errorf("%s: changing the %s: %w", issue.Key, what, err)}
		}
		// The write went through; if the read fails, the row shows what was picked.
		if got, err := jira.issue(ctx, issue.Key); err == nil {
			issue = got
			if kind == pickStatus {
				done = fmt.Sprintf("✓ %s is %s", issue.Key, issue.Status)
			}
		}
		return ticketChangedMsg{issue: issue, done: done}
	}
}

// pickerUpdate handles the picker's messages; handled is false for any other message.
func (m *model) pickerUpdate(msg tea.Msg) (cmd tea.Cmd, handled bool) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		p := m.picker
		if p == nil {
			return nil, false
		}
		if p.busy {
			return nil, true
		}
		p.query += strings.Join(strings.Fields(msg.Content), " ")
		return m.queryChanged(), true
	case pickLoadedMsg:
		if msg.me != nil {
			m.me = msg.me
		}
		p := m.picker
		if p == nil || p.row.Key != msg.key || p.kind != msg.kind || p.seq != msg.seq {
			return nil, true
		}
		p.loading = false
		if msg.err != nil {
			m.say(fmt.Sprintf("%s: %v", msg.key, msg.err), cRed)
			m.statusSticky = true
			return nil, true
		}
		p.items = msg.items
		p.cursor = min(p.cursor, max(0, len(m.choices())-1))
		return nil, true
	case pickSearchMsg:
		if p := m.picker; p != nil && p.seq == msg.seq && !p.busy {
			return m.loadPicks(p.query), true
		}
		return nil, true
	case ticketChangedMsg:
		if p := m.picker; p != nil && p.row.Key == msg.issue.Key {
			p.busy = false
		}
		if msg.err != nil {
			m.say(msg.err.Error(), cRed)
		} else {
			m.replaceIssue(msg.issue)
			if p := m.picker; p != nil && p.row.Key == msg.issue.Key {
				m.picker = nil
			}
			m.say(msg.done, cGreen)
		}
		m.statusSticky = true
		return m.scheduleDetail(), true
	}
	return nil, false
}

// replaceIssue puts the ticket's new values in its row, where it is, and drops its cached detail,
// which shows them too.
func (m *model) replaceIssue(i Issue) {
	for n, r := range m.issues {
		if r.Key == i.Key {
			m.issues[n] = issueRow(i)
		}
	}
	delete(m.details, i.Key)
	delete(m.detailOK, i.Key)
}

// renderPicker draws the picker in rows lines.
func (m *model) renderPicker(width, rows int) []string {
	p := m.picker
	what, now := "Status", p.row.Status
	if p.kind == pickAssignee {
		what, now = "Assignee", cmp.Or(p.row.Assignee, "unassigned")
	}
	out := []string{
		bold(fg(cMauve, what+" of "+p.row.Key)) + fg(cOverlay, " now "+now+" · "+p.row.Summary),
		fg(cMauve, "› ") + p.query + fg(cMauve, "█"),
	}
	items := m.choices()
	p.cursor = max(0, min(p.cursor, len(items)-1))
	room := max(1, rows-len(out))
	top := scrollTo(0, p.cursor, room, len(items))
	for n := top; n < min(len(items), top+room); n++ {
		it := items[n]
		sel := n == p.cursor
		mark := "  "
		if sel {
			mark = fg(cMauve, "▌ ")
		}
		value, color := it.value, cText
		switch it.category {
		case "In Progress":
			color = cYellow
		case "Done":
			color = cGreen
		case "Blocked":
			color = cRed
		}
		if p.kind == pickAssignee && it.me {
			color = cMauve
		}
		if sel {
			value = bold(value)
		}
		line := mark + fg(color, value)
		if it.value == "" {
			line = mark + fg(cSubtext, it.note)
		} else if it.note != "" {
			line += " " + fg(cOverlay, it.note)
		}
		current := strings.EqualFold(it.value, now) || (p.kind == pickAssignee && it.id == "" && p.row.Assignee == "")
		if current {
			line += fg(cSubtext, "  (now)")
		}
		out = append(out, line)
	}
	switch {
	case p.loading && len(items) == 0:
		out = append(out, fg(cOverlay, "  Loading…"))
	case len(items) == 0:
		out = append(out, fg(cOverlay, "  no match"))
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return out[:rows]
}

func (m *model) pickerKeys() string {
	p := m.picker
	switch {
	case p.busy && p.kind == pickStatus:
		return "changing the status…"
	case p.busy:
		return "changing the assignee…"
	case p.loading:
		return "loading… · type to filter · esc back"
	}
	return "↑↓ pick · type to filter · enter set · esc back"
}
