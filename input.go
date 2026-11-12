package main

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

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
			m.mine = !m.mine
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
	case k == "ctrl+s":
		if !ok {
			break
		}
		session := m.state.Sessions[m.agentKey(current.Key)]
		if session == "" || m.agents[session] == "" {
			m.say("No open agent session for "+current.Key, cOverlay)
			break
		}
		return func() tea.Msg {
			if err := switchTo(session); err != nil {
				return statusMsg{"Switching to " + current.Key + "'s session: " + err.Error(), cRed, false}
			}
			return nil
		}
	case k == "ctrl+x":
		return m.findCleanup()
	case k == "ctrl+w":
		return m.openPicker(pickStatus)
	case k == "ctrl+y":
		return m.openPicker(pickAssignee)
	case k == "ctrl+t":
		m.openTaskForm()
	case k == "ctrl+g":
		return func() tea.Msg {
			if err := goToOrchestrator(); err != nil {
				return statusMsg{"Orchestrator: " + err.Error(), cRed, false}
			}
			return nil
		}
	case k == "enter":
		if ok {
			m.openForm(current)
		}
	case k == "backspace":
		m.query = dropLast(m.query)
		move(0)
	case text == "?" && m.query == "":
		m.help = true
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
	if k != "enter" {
		f.confirmed = false
	}
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
	case k == "ctrl+e":
		f.field = fieldContext
		return editContext(f.context)
	case k == "tab" || (k == "down" && f.field != fieldRepo):
		if isPr {
			f.field = prFields[(slices.Index(prFields, f.field)+1)%len(prFields)]
		} else {
			f.field = (f.field + 1) % fieldCount
		}
	case k == "shift+tab" || (k == "up" && f.field != fieldRepo):
		if isPr {
			f.field = prFields[(slices.Index(prFields, f.field)+len(prFields)-1)%len(prFields)]
		} else {
			f.field = (f.field + fieldCount - 1) % fieldCount
		}
	case f.field == fieldAgent && (left || right):
		if left {
			f.agent = (f.agent + len(agents) - 1) % len(agents)
		} else {
			f.agent = (f.agent + 1) % len(agents)
		}
		f.agentPicked = true
	case f.field == fieldWorktree && isPr && (k == "space" || left || right):
		f.fix = !f.fix
	case f.field == fieldWorktree && (k == "space" || left || right):
		f.worktree = !f.worktree
	case f.field == fieldPoteto && (k == "space" || left || right):
		f.poteto = !f.poteto
	case f.field == fieldRepo:
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
		m.followRepo()
	case f.field == fieldContext:
		switch k {
		case "backspace":
			f.context = dropLast(f.context)
		case "alt+enter", "shift+enter":
			f.context += "\n"
		default:
			f.context += text
		}
	}
	return nil
}

// paste adds pasted text to the field being typed in. It arrives as one PasteMsg, not as keys.
func (m *model) paste(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if f := m.form; f != nil && !f.busy {
		switch f.field {
		case fieldRepo:
			f.repoQuery, f.repo = f.repoQuery+strings.Join(strings.Fields(text), " "), 0
		case fieldContext:
			f.context += text
		}
		return
	}
	if m.cleanup == nil {
		m.query += strings.Join(strings.Fields(text), " ")
		m.cursor, m.detailScroll = 0, 0
	}
}

// editContext opens the extra context in $VISUAL or $EDITOR, suspending the board while it runs.
func editContext(text string) tea.Cmd {
	f, err := os.CreateTemp("", "foreman-context-*.md")
	if err != nil {
		return func() tea.Msg { return editedMsg{err: err} }
	}
	path := f.Name()
	_, err = f.WriteString(text)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(path)
		return func() tea.Msg { return editedMsg{err: err} }
	}
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "nvim")
	// Through the shell, so an editor with arguments ("code --wait") works.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer func() { _ = os.Remove(path) }()
		if err != nil {
			return editedMsg{err: fmt.Errorf("%s: %w", editor, err)}
		}
		b, err := os.ReadFile(path)
		return editedMsg{text: strings.TrimRight(string(b), "\n"), err: err}
	})
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
