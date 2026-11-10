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

// A group is where an item sits in the list, most urgent first.
type group int

const (
	needsYou group = iota // an agent that asks something or failed, a PR of mine that's blocked
	toReview              // a finished agent's report I haven't marked reviewed
	working               // an agent still working
	idle                  // an agent with an open session and nothing new for me
	closed                // an agent whose session is closed
)

// A queueItem is one dispatched agent, or one of my PRs that needs me.
type queueItem struct {
	Kind    itemKind
	Group   group
	Key     string
	Title   string
	Agent   string // the agent's reported state, "" when its session is closed
	Session string // the open session's ID, else the recorded one
	Dir     string // the worktree
	Report  string // report.md's path, "" when the agent wrote none
	When    time.Time
	Shots   int // images and terminal captures
	Videos  int
	Pr      *Pr
	// Sig is what the item showed; marking it reviewed holds until this changes.
	Sig string
}

// dismissKey is the item's key in State.Dismissed. A review agent and its PR share a key, so the
// PR's is prefixed: marking one reviewed mustn't overwrite the other's mark.
func (it queueItem) dismissKey() string {
	if it.Kind == prItem {
		return prDismissKey(it.Key)
	}
	return it.Key
}

func prDismissKey(key string) string { return "pr:" + key }

func (it queueItem) review() bool { return strings.Contains(it.Key, "#") }

func (it queueItem) working() bool { return it.Group == working }

// pending is whether the item waits on me: it asks something or has a report to read.
func (it queueItem) pending() bool { return it.Group == needsYou || it.Group == toReview }

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

// queueItems builds the list from the state file, the open Rex sessions and my PRs: every
// dispatched agent, whatever its state, and my PRs blocked on me. Within a group, newest report first.
func queueItems(s State, sessions []rexSession, prs []Pr) []queueItem {
	var items []queueItem
	for key, dir := range s.Dispatched {
		it := queueItem{Kind: agentItem, Key: key, Title: s.Titles[key], Session: s.Sessions[key], Dir: dir}
		if rs, ok := findSession(sessions, key, s.Sessions[key]); ok {
			it.Agent, it.Session = rs.State, rs.ID
		} else if s.Hidden[key] {
			continue
		}
		ev := evidenceDir(key)
		if st, err := os.Stat(filepath.Join(ev, "report.md")); err == nil {
			it.Report, it.When = filepath.Join(ev, "report.md"), st.ModTime()
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
		// Not the state itself: closing a finished agent's session shouldn't bring its report back.
		it.Sig = fmt.Sprintf("%t %d", it.urgent(), it.When.Unix())
		reviewed := s.Dismissed[key] == it.Sig
		switch {
		case it.Agent == "working":
			it.Group = working
		case it.urgent() && !reviewed:
			it.Group = needsYou
		case it.Report != "" && !reviewed:
			it.Group = toReview
		case it.Agent != "":
			it.Group = idle
		default:
			it.Group = closed
		}
		items = append(items, it)
	}
	for _, p := range prs {
		// A PR has no session to keep listing: marked reviewed, it goes until its status changes.
		if p.Mine && p.Category == "Blocked" && s.Dismissed[prDismissKey(p.Key)] != p.Status {
			items = append(items, queueItem{Kind: prItem, Group: needsYou, Key: p.Key, Title: p.Summary, Pr: &p, Sig: p.Status})
		}
	}
	slices.SortFunc(items, func(a, b queueItem) int {
		if d := int(a.Group) - int(b.Group); d != 0 {
			return d
		}
		// Agents that ask before my PRs.
		if d := int(a.Kind) - int(b.Kind); d != 0 {
			return d
		}
		if d := b.When.Compare(a.When); d != 0 {
			return d
		}
		return strings.Compare(a.Key, b.Key)
	})
	return items
}

// sessionFixes are the keys whose open session is no longer the one recorded, by ID: a label that
// the agent renamed, or a dispatch from before IDs were kept. Recording the ID lets the board find it too.
func sessionFixes(s State, sessions []rexSession) map[string]string {
	fixes := map[string]string{}
	for key := range s.Dispatched {
		if rs, ok := findSession(sessions, key, s.Sessions[key]); ok && rs.ID != s.Sessions[key] {
			fixes[key] = rs.ID
		}
	}
	return fixes
}

// staleDismissals are the dismissed keys that can't show again: neither a dispatched ticket or
// review nor one of my PRs still blocked on me. Without pruning, every PR I ever dismissed stays
// in the state file. Called only after a successful PR load, so a failed one prunes nothing.
func staleDismissals(s State, prs []Pr) []string {
	var out []string
	for key := range s.Dismissed {
		if pr, ok := strings.CutPrefix(key, "pr:"); ok {
			if slices.ContainsFunc(prs, func(p Pr) bool { return p.Key == pr && p.Mine && p.Category == "Blocked" }) {
				continue
			}
		} else if _, ok := s.Dispatched[key]; ok {
			continue
		}
		out = append(out, key)
	}
	return out
}

type (
	queueTickMsg  struct{}
	queueStateMsg struct {
		state    State
		sessions []rexSession
		ok       bool // false when listing the sessions failed
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
	sessions      []rexSession
	prs           []Pr
	prsAt         time.Time // the last load attempt, failed or not, so a failing gh isn't retried every poll
	prsLoading    bool
	items         []queueItem
	cursor, top   int
	scroll        int
	details       map[string][]string // by key + sig
	feedback      *reply              // the reply being typed to an agent
	confirmKill   string              // the session the first K asked to close
	status        string
	statusColor   Color
}

// A reply names its agent itself: the list is rebuilt on every poll, and by the time enter is
// pressed the row under the cursor may be another agent's.
type reply struct {
	key, session, text string
}

func detailKey(it queueItem) string { return it.Key + " " + it.Sig }

func (q *queueModel) current() (queueItem, bool) {
	q.cursor = max(0, min(q.cursor, len(q.items)-1))
	if len(q.items) == 0 {
		return queueItem{}, false
	}
	return q.items[q.cursor], true
}

// count is how many items are in the group.
func (q *queueModel) count(g group) int {
	n := 0
	for _, it := range q.items {
		if it.Group == g {
			n++
		}
	}
	return n
}

// needYou counts the items that wait on me: those that ask and the reports to read.
func (q *queueModel) needYou() int { return q.count(needsYou) + q.count(toReview) }

func (q *queueModel) say(text string, c Color) { q.status, q.statusColor = text, c }

func pollQueue() tea.Msg {
	sessions, err := agentSessions()
	return queueStateMsg{loadState(), sessions, err == nil}
}

func (q *queueModel) loadPrs() tea.Cmd {
	if q.prsLoading {
		return nil
	}
	q.prsLoading = true
	return func() tea.Msg {
		prs, _, err := openPrs()
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
	q.items = queueItems(q.state, q.sessions, q.prs)
	if had {
		// The kind too: a review agent and the PR it reviews share a key.
		if n := slices.IndexFunc(q.items, func(it queueItem) bool { return it.Key == prev.Key && it.Kind == prev.Kind }); n >= 0 {
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
	if (it.Report == "" || it.working()) && it.Agent != "" {
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
		if msg.ok {
			q.sessions = msg.sessions
			if fixes := sessionFixes(q.state, q.sessions); len(fixes) > 0 {
				q.state, _ = updateState(func(s *State) {
					for k, id := range fixes {
						s.Sessions[k] = id
					}
				})
			}
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
			q.feedback.text += strings.Join(strings.Fields(msg.Content), " ")
		}
	case tea.KeyPressMsg:
		if q.feedback != nil {
			return q, q.feedbackInput(msg)
		}
		if msg.String() != "K" {
			q.confirmKill = ""
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
		q.feedback = &reply{key: it.Key, session: it.Session}
	case "x":
		if !it.pending() {
			q.say(it.Key+" has nothing waiting for you", cOverlay)
			return nil
		}
		key, dkey, sig := it.Key, it.dismissKey(), it.Sig
		q.state, _ = updateState(func(s *State) { s.Dismissed[dkey] = sig })
		if it.Kind == prItem {
			q.say("Marked "+key+" reviewed; it comes back when its status changes", cOverlay)
		} else {
			q.say("Marked "+key+" reviewed; it moves up again with a new report", cOverlay)
		}
		return q.rebuild()
	case "K":
		if it.Kind == prItem {
			q.say("A PR has no agent session to close", cOverlay)
			return nil
		}
		if it.Agent == "" {
			return q.hide(it)
		}
		// Twice: it ends the agent and whatever else runs in the session.
		if q.confirmKill != it.Session {
			q.confirmKill = it.Session
			warn := ""
			if it.working() {
				warn = " It is still working."
			}
			q.say("K again closes "+it.Key+"'s session."+warn, cYellow)
			return nil
		}
		q.confirmKill = ""
		session, key := it.Session, it.Key
		q.say("Closing "+key+"'s session…", cSubtext)
		kill := func() tea.Msg {
			if err := killSession(session); err != nil {
				return statusMsg{text: "Closing " + key + "'s session: " + err.Error(), color: cRed}
			}
			return statusMsg{text: "✓ Closed " + key + "'s session", color: cGreen}
		}
		// Poll right after, so the row moves to closed now rather than on the next tick.
		return tea.Sequence(kill, pollQueue)
	}
	return nil
}

// hide drops a closed agent from the queue after a second K. Its worktree stays dispatched, so the
// board's ^x can still remove it.
func (q *queueModel) hide(it queueItem) tea.Cmd {
	confirm := "hide " + it.Key
	if q.confirmKill != confirm {
		q.confirmKill = confirm
		q.say("K again drops "+it.Key+" from the queue; its worktree and report stay", cYellow)
		return nil
	}
	q.confirmKill = ""
	key := it.Key
	q.state, _ = updateState(func(s *State) { s.Hidden[key] = true })
	q.say("Dropped "+key+"; ^x on the board cleans up its worktree", cOverlay)
	return q.rebuild()
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
		q.feedback.text = dropLast(q.feedback.text)
	case "enter":
		r := *q.feedback
		q.feedback = nil
		text := strings.TrimSpace(r.text)
		if text == "" {
			return nil
		}
		q.say("Sending to "+r.key+"…", cSubtext)
		return func() tea.Msg {
			if err := sendToAgent(r.session, text); err != nil {
				return statusMsg{text: "Reply to " + r.key + ": " + err.Error(), color: cRed}
			}
			return statusMsg{text: "✓ Sent to " + r.key, color: cGreen}
		}
	default:
		q.feedback.text += msg.Key().Text
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
	// After --: a reply that starts with a dash isn't a flag.
	if _, err := sh("", "rex", "send", "-s", session, "-b", block, "--", text); err != nil {
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
	v.SetContent(frame(q.render(inner, q.height-1), q.status, q.statusColor, inner))
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

var groupNames = map[group]string{needsYou: "needs you", toReview: "review", working: "working", idle: "idle", closed: "closed"}

// itemLabel is the item's coloured mark and its state.
func itemLabel(it queueItem) (string, string) {
	if it.Kind == prItem {
		return fg(cRed, "✗"), fg(cRed, it.Pr.Status)
	}
	what := "done"
	if it.review() {
		what = "reviewed"
	}
	if it.Report == "" {
		what = "no report"
	}
	switch {
	case it.Agent == "working":
		return fg(cYellow, "●"), fg(cYellow, "working")
	case it.Agent == "blocked":
		return fg(cRed, "●"), fg(cRed, "asks you")
	case it.Agent == "error":
		return fg(cRed, "●"), fg(cRed, "failed")
	case it.Group == toReview && it.Agent == "":
		return fg(cGreen, "✓"), fg(cGreen, what) + fg(cOverlay, " (closed)")
	case it.Group == toReview:
		return fg(cGreen, "✓"), fg(cGreen, what)
	case it.Group == idle:
		return fg(cGreen, "○"), fg(cOverlay, what)
	}
	return fg(cOverlay, "◇"), fg(cOverlay, what)
}

func (q *queueModel) render(width, h int) []string {
	dot := fg(cSurface, " · ")
	head := bold(fg(cMauve, "queue")) + dot + fmt.Sprintf("%d need you", q.needYou())
	for _, g := range []struct {
		g group
		c Color
	}{{working, cYellow}, {idle, cGreen}, {closed, cOverlay}} {
		if n := q.count(g.g); n > 0 {
			head += dot + fg(g.c, fmt.Sprintf("%d %s", n, groupNames[g.g]))
		}
	}
	if q.prsLoading {
		head += fg(cYellow, "  loading PRs…")
	}
	out := []string{head}

	listRows := max(3, min(len(q.items), (h-3)*40/100))
	detailRows := h - listRows - 3
	q.top = scrollTo(q.top, q.cursor, listRows, len(q.items))

	keyW := 8
	for _, it := range q.items {
		keyW = max(keyW, len(it.Key))
	}
	keyW = min(22, keyW)
	for n := q.top; n < q.top+listRows; n++ {
		if n >= len(q.items) {
			empty := ""
			if n == 0 {
				empty = fg(cOverlay, "  no agents dispatched")
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
		// The group's name on its first row, and on the top row when the group starts above it.
		name := ""
		if n == q.top || q.items[n-1].Group != it.Group {
			name = groupNames[it.Group]
		}
		groupColor := cOverlay
		if it.pending() {
			groupColor = cMauve
		}
		line += fg(groupColor, fit(name, 9)) + " "
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
		if it.Group == closed {
			title = fg(cOverlay, it.Title)
		}
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
		detail = append(detail, bold(fg(cMauve, "Reply to "+q.feedback.key)), "")
		for _, l := range wrap(q.feedback.text+"█", width-2) {
			detail = append(detail, "  "+l)
		}
	} else if it, ok := q.current(); ok {
		lines := q.details[detailKey(it)]
		if lines == nil {
			lines = []string{"# " + it.Key + ": " + it.Title, "", fg(cOverlay, "Loading…")}
		}
		detail = renderMarkdown(lines, width)
	}
	out = append(out, window(detail, &q.scroll, detailRows)...)

	keys := "enter go · f reply · v evidence · d diff · x reviewed · K close/drop · o open · ^u/^d scroll"
	if q.feedback != nil {
		keys = "type the reply · enter send · esc cancel"
	}
	return append(out, fg(cOverlay, keys))
}

func runQueue() error {
	q := &queueModel{
		details:     map[string][]string{},
		statusColor: cOverlay,
		state:       loadState(),
	}
	_, err := tea.NewProgram(q).Run()
	return err
}
