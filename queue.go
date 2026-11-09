package main

// foreman queue: what needs me, one item at a time. Agents that stopped to ask something or
// finished (with their report and evidence), and my PRs whose CI failed, got changes requested or
// conflict. It runs in its own Rex pane beside the board and reads the board's state file.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const queuePoll = 3 * time.Second

type itemKind int

const (
	agentItem itemKind = iota
	prItem
)

// A queueItem is one thing waiting for me.
type queueItem struct {
	Kind    itemKind
	Key     string
	Title   string
	Agent   string // the agent's reported state, "" when its session is closed
	Session string
	Dir     string // the worktree
	Report  string // report.md's path, "" when the agent wrote none
	When    time.Time
	Shots   int // images and terminal captures
	Videos  int
	Pr      *Pr
	// Sig is what the item showed; dismissing it hides it until this changes.
	Sig string
}

func (it queueItem) review() bool { return strings.Contains(it.Key, "#") }

func (it queueItem) working() bool { return it.Kind == agentItem && it.Agent == "working" }

// urgent is whether the item is blocked on me rather than done.
func (it queueItem) urgent() bool {
	return it.Kind == prItem || it.Agent == "blocked" || it.Agent == "error"
}

var (
	imageExt = []string{".png", ".jpg", ".jpeg", ".gif", ".webp"}
	videoExt = []string{".webm", ".mp4", ".mov"}
)

// evidence lists the media in dir, images and videos first, then terminal captures.
func evidence(dir string) (media, captures []string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		p := filepath.Join(dir, e.Name())
		switch {
		case slices.Contains(imageExt, ext), slices.Contains(videoExt, ext):
			media = append(media, p)
		case ext == ".txt":
			captures = append(captures, p)
		}
	}
	return media, captures
}

// queueItems builds the queue from the state file, the agents' reported states and my PRs.
// Blocked agents come first, then my PRs that need me, then finished work, newest first.
func queueItems(s State, agents map[string]string, prs []Pr) []queueItem {
	var items []queueItem
	for key, dir := range s.Dispatched {
		session := s.Sessions[key]
		state, open := agents[session]
		if !open {
			state = ""
		}
		it := queueItem{Kind: agentItem, Key: key, Title: s.Titles[key], Agent: state, Session: session, Dir: dir}
		// A working agent is listed last, to follow along; it doesn't need me, so it can't be dismissed.
		if it.working() {
			it.Sig = "working"
			items = append(items, it)
			continue
		}
		ev := evidenceDir(key)
		if st, err := os.Stat(filepath.Join(ev, "report.md")); err == nil {
			it.Report, it.When = filepath.Join(ev, "report.md"), st.ModTime()
		}
		// Done work shows only with a report; an agent that stopped to ask shows either way.
		if it.Report == "" && !it.urgent() {
			continue
		}
		media, captures := evidence(ev)
		for _, p := range media {
			if slices.Contains(videoExt, strings.ToLower(filepath.Ext(p))) {
				it.Videos++
			} else {
				it.Shots++
			}
		}
		it.Shots += len(captures)
		// Not the state itself: closing a finished agent's session shouldn't bring its item back.
		it.Sig = fmt.Sprintf("%t %d", it.urgent(), it.When.Unix())
		items = append(items, it)
	}
	for _, p := range prs {
		if p.Mine && p.Category == "Blocked" {
			items = append(items, queueItem{Kind: prItem, Key: p.Key, Title: p.Summary, Pr: &p, Sig: p.Status})
		}
	}
	items = slices.DeleteFunc(items, func(it queueItem) bool { return !it.working() && s.Dismissed[it.Key] == it.Sig })
	rank := func(it queueItem) int {
		switch {
		case it.Kind == agentItem && it.urgent():
			return 0
		case it.Kind == prItem:
			return 1
		case it.working():
			return 3
		}
		return 2
	}
	slices.SortFunc(items, func(a, b queueItem) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		if d := b.When.Compare(a.When); d != 0 {
			return d
		}
		return strings.Compare(a.Key, b.Key)
	})
	return items
}

// staleDismissals are the dismissed keys that can't show again: neither a dispatched ticket or
// review nor one of my PRs still blocked on me. Without pruning, every PR I ever dismissed stays
// in the state file. Called only after a successful PR load, so a failed one prunes nothing.
func staleDismissals(s State, prs []Pr) []string {
	var out []string
	for key := range s.Dismissed {
		if _, ok := s.Dispatched[key]; ok {
			continue
		}
		if slices.ContainsFunc(prs, func(p Pr) bool { return p.Key == key && p.Mine && p.Category == "Blocked" }) {
			continue
		}
		out = append(out, key)
	}
	return out
}

type (
	queueTickMsg  struct{}
	queueStateMsg struct {
		state  State
		agents map[string]string
	}
	queuePrsMsg struct {
		prs []Pr
		err error
	}
	queueDetailMsg struct {
		sig   string
		lines []string
	}
)

type queueModel struct {
	width, height int
	state         State
	agents        map[string]string
	prs           []Pr
	prsAt         time.Time // the last load attempt, failed or not, so a failing gh isn't retried every poll
	prsLoading    bool
	items         []queueItem
	cursor, top   int
	scroll        int
	details       map[string][]string // by key + sig
	feedback      *string             // the reply being typed to the selected agent
	status        string
	statusColor   Color
}

func detailKey(it queueItem) string { return it.Key + " " + it.Sig }

func (q *queueModel) current() (queueItem, bool) {
	q.cursor = max(0, min(q.cursor, len(q.items)-1))
	if len(q.items) == 0 {
		return queueItem{}, false
	}
	return q.items[q.cursor], true
}

// needYou counts the items that wait on me: all but the working agents.
func (q *queueModel) needYou() int {
	n := 0
	for _, it := range q.items {
		if !it.working() {
			n++
		}
	}
	return n
}

func (q *queueModel) say(text string, c Color) { q.status, q.statusColor = text, c }

func pollQueue() tea.Msg {
	agents, _ := agentStates()
	return queueStateMsg{loadState(), agents}
}

func (q *queueModel) loadPrs() tea.Cmd {
	if q.prsLoading {
		return nil
	}
	q.prsLoading = true
	return func() tea.Msg {
		prs, err := openPrs()
		return queuePrsMsg{prs, err}
	}
}

func (q *queueModel) Init() tea.Cmd { return tea.Batch(pollQueue, queueTick(), q.loadPrs()) }

// queueTick schedules the next poll. Only queueTickMsg reschedules it, so ^r doesn't start another loop.
func queueTick() tea.Cmd {
	return tea.Tick(queuePoll, func(time.Time) tea.Msg { return queueTickMsg{} })
}

// rebuild recomputes the items, keeping the cursor on the same item when it is still there.
func (q *queueModel) rebuild() tea.Cmd {
	prev, had := q.current()
	q.items = queueItems(q.state, q.agents, q.prs)
	if had {
		if n := slices.IndexFunc(q.items, func(it queueItem) bool { return it.Key == prev.Key }); n >= 0 {
			q.cursor = n
		}
	}
	return q.loadDetail()
}

func (q *queueModel) loadDetail() tea.Cmd {
	it, ok := q.current()
	if !ok {
		return nil
	}
	key := detailKey(it)
	// A working agent's screen changes, so it is read again on every poll, keeping the old one meanwhile.
	if _, cached := q.details[key]; cached && !it.working() {
		return nil
	} else if !cached {
		q.details[key] = nil
	}
	return func() tea.Msg { return queueDetailMsg{key, itemDetail(it)} }
}

// itemDetail is the item's report, its evidence and terminal captures, or for an agent that wrote
// no report, the end of its screen, where its question is.
func itemDetail(it queueItem) []string {
	if it.Kind == prItem {
		v, err := viewPr(*it.Pr)
		if err != nil {
			return []string{fg(cRed, "Failed to load the PR: "+err.Error())}
		}
		return strings.Split(prMarkdown(*it.Pr, v), "\n")
	}
	var out []string
	if it.Report != "" {
		b, err := os.ReadFile(it.Report)
		if err != nil {
			out = append(out, fg(cRed, err.Error()))
		}
		out = append(out, strings.Split(strings.TrimRight(string(b), "\n"), "\n")...)
	}
	media, captures := evidence(evidenceDir(it.Key))
	if len(media) > 0 {
		out = append(out, "", "## Evidence", "")
		for _, p := range media {
			out = append(out, "- `"+filepath.Base(p)+"`")
		}
	}
	for _, p := range captures {
		b, _ := os.ReadFile(p)
		out = append(out, "", "## "+filepath.Base(p), "")
		out = append(out, strings.Split(strings.TrimRight(string(b), "\n"), "\n")...)
	}
	if (it.Report == "" || it.working()) && it.Session != "" {
		screen, err := sh("", "rex", "capture", "-s", it.Session, "--trim")
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, "## Its screen", "")
		if err != nil {
			out = append(out, fg(cRed, firstLine(err.Error())))
		} else {
			lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
			out = append(out, lines[max(0, len(lines)-40):]...)
		}
	}
	return out
}

func (q *queueModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		q.width, q.height = msg.Width, msg.Height
	case queueTickMsg:
		cmds := []tea.Cmd{pollQueue, queueTick()}
		if time.Since(q.prsAt) >= refreshInterval {
			cmds = append(cmds, q.loadPrs())
		}
		return q, tea.Batch(cmds...)
	case queueStateMsg:
		q.state = msg.state
		if msg.agents != nil {
			q.agents = msg.agents
		}
		return q, q.rebuild()
	case queuePrsMsg:
		q.prsLoading, q.prsAt = false, time.Now()
		if msg.err != nil {
			q.say("PRs: "+firstLine(msg.err.Error()), cRed)
			break
		}
		q.prs = msg.prs
		if stale := staleDismissals(q.state, q.prs); len(stale) > 0 {
			q.state, _ = updateState(func(s *State) {
				for _, k := range stale {
					delete(s.Dismissed, k)
				}
			})
		}
		return q, q.rebuild()
	case queueDetailMsg:
		q.details[msg.sig] = msg.lines
	case statusMsg:
		q.say(msg.text, msg.color)
	case tea.FocusMsg:
		if time.Since(q.prsAt) >= refreshInterval {
			return q, q.loadPrs()
		}
	case tea.PasteMsg:
		if q.feedback != nil {
			*q.feedback += strings.Join(strings.Fields(msg.Content), " ")
		}
	case tea.KeyPressMsg:
		if q.feedback != nil {
			return q, q.feedbackInput(msg)
		}
		return q, q.input(msg)
	}
	return q, nil
}

func (q *queueModel) input(msg tea.KeyPressMsg) tea.Cmd {
	it, ok := q.current()
	move := func(to int) tea.Cmd {
		q.cursor, q.scroll = max(0, to), 0
		return q.loadDetail()
	}
	switch msg.String() {
	// No esc: the pane is meant to stay, and closing it takes the pane away.
	case "ctrl+c", "q":
		return tea.Quit
	case "up", "k", "ctrl+p":
		return move(q.cursor - 1)
	case "down", "j", "ctrl+n":
		return move(q.cursor + 1)
	case "ctrl+u":
		q.scroll -= 10
	case "ctrl+d", "space":
		q.scroll += 10
	case "ctrl+r":
		q.details = map[string][]string{}
		return tea.Batch(pollQueue, q.loadPrs())
	}
	if !ok {
		return nil
	}
	switch msg.String() {
	case "enter", "s":
		if it.Kind == prItem {
			openURL(it.Pr.URL)
			return nil
		}
		if it.Agent == "" {
			q.say("Its session is closed", cOverlay)
			return nil
		}
		session := it.Session
		return func() tea.Msg {
			if err := switchTo(session); err != nil {
				return statusMsg{text: "Switching to " + it.Key + "'s session: " + err.Error(), color: cRed}
			}
			return nil
		}
	case "v":
		if it.Kind == prItem {
			return nil
		}
		dir := evidenceDir(it.Key)
		media, _ := evidence(dir)
		if len(media) == 0 {
			q.say("No screenshots or video for "+it.Key, cOverlay)
			return nil
		}
		// Images open together in one Preview window; videos in the default player.
		_ = exec.Command("open", media...).Start()
	case "d":
		if it.Kind == agentItem {
			return q.openDiff(it)
		}
	case "o":
		switch {
		case it.Pr != nil:
			openURL(it.Pr.URL)
		case it.review():
			if n := slices.IndexFunc(q.prs, func(p Pr) bool { return p.Key == it.Key }); n >= 0 {
				openURL(q.prs[n].URL)
			}
		default:
			openURL(site + "/browse/" + it.Key)
		}
	case "f":
		if it.Kind == prItem || it.Agent == "" {
			q.say("No open agent session to reply to", cOverlay)
			return nil
		}
		empty := ""
		q.feedback = &empty
	case "x":
		if it.working() {
			q.say(it.Key+" is still working; it shows here until it stops", cOverlay)
			return nil
		}
		key, sig := it.Key, it.Sig
		q.state, _ = updateState(func(s *State) { s.Dismissed[key] = sig })
		q.say("Dismissed "+key+"; it comes back when it changes", cOverlay)
		return q.rebuild()
	}
	return nil
}

// openDiff shows the worktree's changes in a pager, suspending the queue: a ticket's against where
// its branch forked plus anything uncommitted, a review's against the PR's base.
func (q *queueModel) openDiff(it queueItem) tea.Cmd {
	if !exists(it.Dir) {
		q.say(tilde(it.Dir)+" is gone", cOverlay)
		return nil
	}
	args := []string{"-C", it.Dir, "diff"}
	if base := diffBase(it.Dir, it.review()); base != "" {
		if mb, err := sh(it.Dir, "git", "merge-base", base, "HEAD"); err == nil {
			args = append(args, strings.TrimSpace(mb))
		}
	}
	return tea.ExecProcess(exec.Command("git", args...), func(err error) tea.Msg {
		if err != nil {
			return statusMsg{text: "git diff: " + err.Error(), color: cRed}
		}
		return nil
	})
}

// diffBase is the branch the work started from: a ticket branch's upstream when it tracks one,
// else develop or origin's default branch. A review worktree is detached, so it is the latter.
func diffBase(dir string, review bool) string {
	if !review {
		if up, err := sh(dir, "git", "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
			return strings.TrimSpace(up)
		}
	}
	out, _ := sh(dir, "git", "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")
	head, _ := sh(dir, "git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	return baseBranch(strings.Fields(out), strings.TrimSpace(head))
}

func (q *queueModel) feedbackInput(msg tea.KeyPressMsg) tea.Cmd {
	switch k := msg.String(); k {
	case "esc":
		q.feedback = nil
	case "ctrl+c":
		return tea.Quit
	case "backspace":
		*q.feedback = dropLast(*q.feedback)
	case "enter":
		text := strings.TrimSpace(*q.feedback)
		q.feedback = nil
		it, ok := q.current()
		if text == "" || !ok {
			return nil
		}
		q.say("Sending to "+it.Key+"…", cSubtext)
		return func() tea.Msg {
			if err := sendToAgent(it.Session, text); err != nil {
				return statusMsg{text: "Reply to " + it.Key + ": " + err.Error(), color: cRed}
			}
			return statusMsg{text: "✓ Sent to " + it.Key, color: cGreen}
		}
	default:
		*q.feedback += msg.Key().Text
	}
	return nil
}

// agentBlockScript finds the session's pane whose program reports a status (pi or Claude Code),
// else its first placed pane: a session can hold a shell split beside the agent.
const agentBlockScript = attachLua + `
local label = %s
for _, s in ipairs(rex.call("session.list").sessions or {}) do
  if s.session_id == label or s.label == label then
    attach(s.session_id)
    local first
    for _, b in ipairs((rex.call("session.list_blocks", {session_id = s.session_id}) or {}).blocks or {}) do
      if b.placement == "placed" then
        local ok, st = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "program_status"})
        if ok and st and #(st.records or {}) > 0 then return b.block_id end
        first = first or b.block_id
      end
    end
    if first then return first end
    error("no pane in session " .. label, 0)
  end
end
error("no session " .. label, 0)
`

// sendToAgent types text into the agent's pane and presses enter, as if I had replied there.
func sendToAgent(session, text string) error {
	out, err := sh("", "rex", "do", "--all", "-e", fmt.Sprintf(agentBlockScript, strconv.Quote(session)), "--json")
	if err != nil {
		return err
	}
	var block string
	if err := json.Unmarshal([]byte(out), &block); err != nil {
		return fmt.Errorf("finding the agent's pane: %w", err)
	}
	if _, err := sh("", "rex", "send", "-s", session, "-b", block, text); err != nil {
		return err
	}
	// A separate key press: in one write the agent's editor takes the newline as pasted text.
	time.Sleep(150 * time.Millisecond)
	_, err = sh("", "rex", "send-key", "-s", session, "-b", block, "enter")
	return err
}

func (q *queueModel) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.ReportFocus = true
	v.WindowTitle = "queue"
	if n := q.needYou(); n > 0 {
		v.WindowTitle = fmt.Sprintf("queue (%d)", n)
	}
	if q.width == 0 {
		return v
	}
	inner := q.width - 2
	body := q.render(inner, q.height-1)
	lines := make([]string, 0, len(body)+1)
	for _, l := range body {
		lines = append(lines, " "+fit(l, inner))
	}
	lines = append(lines, " "+fit(fg(q.statusColor, q.status), inner))
	v.SetContent(strings.Join(lines, "\n"))
	return v
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

// itemLabel is the item's coloured mark and what it is waiting on.
func itemLabel(it queueItem) (string, string) {
	if it.Kind == prItem {
		return fg(cRed, "✗"), fg(cRed, it.Pr.Status)
	}
	what := "done"
	if it.review() {
		what = "reviewed"
	}
	switch it.Agent {
	case "working":
		return fg(cYellow, "●"), fg(cYellow, "working")
	case "blocked":
		return fg(cRed, "●"), fg(cRed, "asks you")
	case "error":
		return fg(cRed, "●"), fg(cRed, "failed")
	case "":
		return fg(cGreen, "✓"), fg(cGreen, what) + fg(cOverlay, " (closed)")
	}
	return fg(cGreen, "✓"), fg(cGreen, what)
}

func (q *queueModel) render(width, h int) []string {
	working := len(q.items) - q.needYou()
	dot := fg(cSurface, " · ")
	head := bold(fg(cMauve, "queue")) + dot + fmt.Sprintf("%d need you", q.needYou())
	if working > 0 {
		head += dot + fg(cYellow, fmt.Sprintf("%d working", working))
	}
	if q.prsLoading {
		head += fg(cYellow, "  loading PRs…")
	}
	out := []string{head}

	listRows := max(3, min(len(q.items), (h-3)*40/100))
	detailRows := h - listRows - 3
	if q.cursor < q.top {
		q.top = q.cursor
	}
	if q.cursor >= q.top+listRows {
		q.top = q.cursor - listRows + 1
	}
	q.top = max(0, min(q.top, max(0, len(q.items)-listRows)))

	keyW := 8
	for _, it := range q.items {
		keyW = max(keyW, len(it.Key))
	}
	keyW = min(22, keyW)
	for n := q.top; n < q.top+listRows; n++ {
		if n >= len(q.items) {
			empty := ""
			if n == 0 {
				empty = fg(cOverlay, "  nothing needs you")
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
		mark, what := itemLabel(it)
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
		if a := age(it.When); a != "" {
			meta += "  " + fg(cOverlay, a)
		}
		keyColor, title := cOverlay, fg(cSubtext, it.Title)
		if sel {
			keyColor, title = cText, bold(fg(cText, it.Title))
		}
		left := line + mark + " " + fg(keyColor, fit(it.Key, keyW)) + "  "
		titleW := width - ansi.StringWidth(left) - ansi.StringWidth(meta) - 2
		out = append(out, left+fit(title, max(0, titleW))+"  "+meta)
	}
	out = append(out, fg(cSurface, strings.Repeat("─", width)))

	var detail []string
	if q.feedback != nil {
		it, _ := q.current()
		detail = append(detail, bold(fg(cMauve, "Reply to "+it.Key)), "")
		for _, l := range wrap(*q.feedback+"█", width-2) {
			detail = append(detail, "  "+l)
		}
	} else if it, ok := q.current(); ok {
		lines := q.details[detailKey(it)]
		if lines == nil {
			lines = []string{"# " + it.Key + ": " + it.Title, "", fg(cOverlay, "Loading…")}
		}
		for _, l := range lines {
			if l == "" {
				detail = append(detail, "")
			} else {
				detail = append(detail, wrap(markdownLine(l), width)...)
			}
		}
	}
	q.scroll = max(0, min(q.scroll, len(detail)-detailRows))
	for n := 0; n < detailRows; n++ {
		if at := q.scroll + n; at < len(detail) {
			out = append(out, detail[at])
		} else {
			out = append(out, "")
		}
	}

	keys := "enter go · f reply · v evidence · d diff · x dismiss · o open · ^u/^d scroll"
	if q.feedback != nil {
		keys = "type the reply · enter send · esc cancel"
	}
	return append(out, fg(cOverlay, keys))
}

func runQueue() error {
	q := &queueModel{
		agents:      map[string]string{},
		details:     map[string][]string{},
		statusColor: cOverlay,
		state:       loadState(),
	}
	_, err := tea.NewProgram(q).Run()
	return err
}
