package main

// foreman queue: what needs me, one item at a time. Agents that stopped to ask something or
// finished (with their report and evidence), my PRs whose CI failed, got changes requested or
// conflict, and my approved PRs, ready to merge. It is drawn beside the board, by the app, and shares the state file with it.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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
	toReview              // a finished agent's report I haven't marked reviewed, an approved PR of mine
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
	Doing   string // what the agent says it is doing, when it reports that
	Session string // the open session's ID, else the recorded one
	Block   string // the agent's pane in that session, "" when it is closed
	Waits   string // what a blocked agent says it waits for: permission, question or auth
	Dir     string // the worktree
	Report  string // report.md's path, "" when the agent wrote none
	When    time.Time
	Shots   int // images and terminal captures
	Videos  int
	Pr      *Pr
	// Watcher marks a watcher's session rather than a dispatched agent's.
	Watcher bool
	// Polled marks a watcher the board wakes when its PRs change, rather than one that loops.
	Polled bool
	// Orchestrator marks the orchestrator's session.
	Orchestrator bool
	// Question is the one the agent waits on in foreman ask.
	Question *Question
	// Sig is what the item showed; marking it reviewed holds until this changes.
	Sig string
	// Fold marks the row that stands for the closed agents while they are folded, and counts them.
	Fold int
}

// on puts the item on its open session: the agent's state, its pane, and what it says it is doing
// or waits for.
func (it *queueItem) on(s rexSession) {
	it.Agent, it.Session, it.Block, it.Doing, it.Waits = s.State, s.ID, s.Block, s.Msg, s.Kind
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

func (it queueItem) review() bool { return strings.Contains(it.Key, "#") && !isFix(it.Key) }

func (it queueItem) working() bool { return it.Group == working }

// pending is whether the item waits on me: it asks something or has a report to read.
func (it queueItem) pending() bool { return it.Group == needsYou || it.Group == toReview }

// urgent is whether the item is blocked on me rather than done: an agent that asks, failed, or
// waits in foreman ask on a question handed to me.
func (it queueItem) urgent() bool {
	if it.Kind == prItem {
		return it.Pr.Category == "Blocked"
	}
	return it.Agent == "blocked" || it.Agent == "error" || (it.Question != nil && it.Question.Escalated != "")
}

// queuedPr is whether the PR waits on me: mine and blocked, or mine and approved (the board files
// an approved PR whose CI isn't running under Done).
func queuedPr(p Pr) bool {
	return p.Mine && (p.Category == "Blocked" || p.Category == "Done")
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
			it.on(rs)
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
		if q, ok := s.Questions[key]; ok && q.Answer == "" {
			it.Question = &q
			if q.Escalated != "" {
				it.Sig += fmt.Sprintf(" q%d", q.At.Unix())
			}
		}
		reviewed := s.Dismissed[key] == it.Sig
		switch {
		case it.urgent() && !reviewed:
			it.Group = needsYou
		case it.Agent == "working":
			it.Group = working
		case it.Report != "" && !reviewed:
			it.Group = toReview
		case it.Agent != "":
			it.Group = idle
		default:
			it.Group = closed
		}
		items = append(items, it)
	}
	for _, w := range watchers {
		items = append(items, watcherItem(w, sessions, s.WatcherSessions[w.Name]))
	}
	if it, ok := orchestratorItem(sessions, s.Orchestrator); ok {
		items = append(items, it)
	}
	for _, p := range prs {
		// A PR has no session to keep listing: marked reviewed, it goes until its status changes.
		if queuedPr(p) && s.Dismissed[prDismissKey(p.Key)] != p.Status {
			it := queueItem{Kind: prItem, Group: toReview, Key: p.Key, Title: p.Summary, Pr: &p, Sig: p.Status}
			if it.urgent() {
				it.Group = needsYou
			}
			items = append(items, it)
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
// review nor one of my PRs still blocked on me or approved. Without pruning, every PR I ever dismissed stays
// in the state file. Called only after a successful PR load, so a failed one prunes nothing.
func staleDismissals(s State, prs []Pr) []string {
	var out []string
	for key := range s.Dismissed {
		if pr, ok := strings.CutPrefix(key, "pr:"); ok {
			if slices.ContainsFunc(prs, func(p Pr) bool { return p.Key == pr && queuedPr(p) }) {
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
	// queueStatusMsg is a line for the queue's status bar; the board's own is statusMsg.
	queueStatusMsg struct {
		text  string
		color Color
	}
	// openFormMsg asks the board to open its dispatch form on a ticket.
	openFormMsg    struct{ key string }
	queueDetailMsg struct {
		sig   string
		lines []string
		seen  string // the seen token of the prompt the lines show, for an agent that waits on one
	}
)

type queueModel struct {
	width, height int
	state         State
	sessions      []rexSession
	prs           []Pr
	prsAt         time.Time // the last load attempt, failed or not, so a failing gh isn't retried every poll
	prsLoading    bool
	// reloadPrs, when set, has the board load the PRs, which then reach the queue too: beside the
	// board, the queue loads none of its own.
	reloadPrs   func() tea.Cmd
	all         []queueItem // every item
	items       []queueItem // the rows: all, with the closed agents folded into one unless showClosed
	showClosed  bool
	cursor, top int
	scroll      int
	details     map[string][]string // by key + sig
	feedback    *reply              // the reply being typed to an agent
	confirmKill string              // the session the first K asked to close
	seen        map[string]string   // by key + sig: the seen token of the prompt a detail shows
	reported    string              // the last status reported to Rex
	records     map[string]string   // the last record reported to Rex for each item that waits on me
	statusAt    time.Time           // when the status line was set: it clears after statusTTL
	help        bool                // the list of keys is showing
	helpScroll  int
	theme       theme
	status      string
	statusColor Color
}

// A reply names its agent itself: the list is rebuilt on every poll, and by the time enter is
// pressed the row under the cursor may be another agent's.
type reply struct {
	key, session, block, text string
	back                      int  // the text's cursor, in runes from its end
	answer                    bool // the agent waits in foreman ask: the text is its answer
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
	for _, it := range q.all {
		if it.Group == g {
			n++
		}
	}
	return n
}

// needYou counts the items that wait on me: those that ask and the reports to read.
func (q *queueModel) needYou() int { return q.count(needsYou) + q.count(toReview) }

func (q *queueModel) say(text string, c Color) {
	q.status, q.statusColor, q.statusAt = text, c, time.Now()
}

// pollQueue reads the state file and the sessions: from Rex's events while they arrive, which
// costs no call, else by listing them.
func pollQueue() tea.Msg {
	sessions, err := agentSessions()
	return queueStateMsg{loadState(), sessions, err == nil}
}

// foldClosed is the rows of items: the closed agents become one row that counts them, unless show.
// What is closed has nothing for me, and a list of it buries what has. A stopped watcher stays:
// enter starts it.
func foldClosed(items []queueItem, show bool) []queueItem {
	if show {
		return items
	}
	var out []queueItem
	n := 0
	for _, it := range items {
		if it.Group == closed && !it.Watcher {
			n++
			continue
		}
		out = append(out, it)
	}
	if n > 0 {
		out = append(out, queueItem{Kind: agentItem, Group: closed, Fold: n, Sig: "fold"})
	}
	return out
}

// foldDetail lists the closed agents the fold stands for.
func foldDetail(items []queueItem) []string {
	out := []string{"# Closed agents", "",
		"`c` lists them here, where `enter` resumes one and `K` twice drops it. The board drops one whose ticket is Done or whose PR is merged or closed, and `^x` there cleans up the worktrees.", ""}
	for _, it := range items {
		if it.Group == closed && !it.Watcher {
			_, what := itemLabel(it)
			line := "- `" + it.Key + "` " + it.Title + fg(cSurface, " · ") + what
			if a := age(it.When); a != "" {
				line += fg(cSurface, " · ") + fg(cOverlay, a)
			}
			out = append(out, line)
		}
	}
	return out
}

func (q *queueModel) loadPrs() tea.Cmd {
	if q.prsLoading {
		return nil
	}
	if q.reloadPrs != nil {
		// Asked once: the board's load answers, or its own refresh does.
		q.prsAt = time.Now()
		return q.reloadPrs()
	}
	q.prsLoading = true
	return func() tea.Msg {
		prs, _, err := openPrs()
		return queuePrsMsg{prs, err}
	}
}

func (q *queueModel) Init() tea.Cmd {
	return tea.Batch(pollQueue, queueTick(), q.loadPrs(), requestTheme())
}

// queueTick schedules the next poll. Only queueTickMsg reschedules it, so ^r doesn't start another loop.
func queueTick() tea.Cmd {
	return tea.Tick(queuePoll, func(time.Time) tea.Msg { return queueTickMsg{} })
}

// rebuild recomputes the items, keeping the cursor on the same item when it is still there.
func (q *queueModel) rebuild() tea.Cmd {
	prev, had := q.current()
	q.all = queueItems(q.state, q.sessions, q.prs)
	q.items = foldClosed(q.all, q.showClosed)
	if had {
		// The kind too: a review agent and the PR it reviews share a key.
		if n := slices.IndexFunc(q.items, func(it queueItem) bool { return it.Key == prev.Key && it.Kind == prev.Kind }); n >= 0 {
			q.cursor = n
		}
	}
	return tea.Batch(q.loadDetail(), q.report())
}

func (q *queueModel) loadDetail() tea.Cmd {
	it, ok := q.current()
	if !ok {
		return nil
	}
	key := detailKey(it)
	if it.Fold > 0 {
		q.details[key] = foldDetail(q.all)
		return nil
	}
	// A working agent's screen changes, as does the prompt an agent waits on, so they are read
	// again on every poll, keeping the old one meanwhile.
	prompt := it.Agent == "blocked"
	if _, cached := q.details[key]; cached && !it.working() && !prompt {
		return nil
	} else if !cached {
		q.details[key] = nil
	}
	return func() tea.Msg {
		if prompt {
			if asks, seen := promptDetail(it, capturePane); seen != "" {
				return queueDetailMsg{key, append(asks, itemDetail(it)...), seen}
			}
		}
		return queueDetailMsg{key, itemDetail(it), ""}
	}
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
	if q := it.Question; q != nil {
		out = append(out, "# Waiting on an answer", "", q.Text, "")
		if q.Escalated != "" {
			out = append(out, "**The orchestrator suggests:** "+q.Escalated, "")
		}
		out = append(out, "`f` answers it.", "")
	}
	if it.Report != "" {
		b, err := os.ReadFile(it.Report)
		if err != nil {
			out = append(out, fg(cRed, err.Error()))
		}
		out = append(out, strings.Split(strings.TrimRight(string(b), "\n"), "\n")...)
	}
	if log := logLines(readLog(it.Key)); log != nil {
		out = append(append(out, ""), log...)
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
		screen, err := rex.capture(it.Session, it.Block, true)
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, "## Its screen", "")
		if err != nil {
			out = append(out, fg(cRed, firstLine(err.Error())))
		} else {
			lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
			// A prompt lives in the agent's own frame, so a blocked agent's screen keeps it.
			if it.Agent != "blocked" {
				lines = withoutInputBox(lines)
			}
			out = append(out, lines[max(0, len(lines)-40):]...)
		}
	}
	return out
}

func (q *queueModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if q.theme.take(msg) {
		return q, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		q.width, q.height = msg.Width, msg.Height
	case queueTickMsg:
		if q.status != "" && time.Since(q.statusAt) > statusTTL(q.statusColor) {
			q.say("", cOverlay)
			q.confirmKill = ""
		}
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
	case rexEventMsg:
		return q, pollQueue
	case queueDetailMsg:
		q.details[msg.sig] = msg.lines
		if q.seen == nil {
			q.seen = map[string]string{}
		}
		q.seen[msg.sig] = msg.seen
	case queueStatusMsg:
		q.say(msg.text, msg.color)
	case tea.FocusMsg:
		if time.Since(q.prsAt) >= refreshInterval {
			return q, q.loadPrs()
		}
	case tea.PasteMsg:
		if q.feedback != nil {
			q.feedback.text = insertText(q.feedback.text, q.feedback.back, strings.Join(strings.Fields(msg.Content), " "))
		}
	case tea.KeyPressMsg:
		if q.help {
			q.help = helpInput(msg, &q.helpScroll)
			return q, nil
		}
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
	// No esc: it is easy to press, and quitting takes the board with it.
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
	case "?":
		q.help = true
		return nil
	case "c":
		q.showClosed = !q.showClosed
		return q.rebuild()
	}
	if !ok {
		return nil
	}
	if it.Fold > 0 {
		if k := msg.String(); k == "enter" || k == "s" {
			q.showClosed = true
			return q.rebuild()
		}
		return nil
	}
	switch msg.String() {
	case "y", "n":
		seen := q.seen[detailKey(it)]
		if it.Kind == agentItem && it.Agent == "blocked" && !pressable(it) {
			q.say(it.Key+" "+waitsFor(it.Waits)+": enter goes to its session, f types a reply", cOverlay)
			return nil
		}
		if !pressable(it) || seen == "" {
			q.say(it.Key+" shows no prompt to answer here", cOverlay)
			return nil
		}
		key, did := "enter", "✓ Pressed enter in "
		if msg.String() == "n" {
			key, did = "escape", "✓ Pressed esc in "
		}
		// Read again whatever happens: the prompt is gone, or it wasn't the one shown.
		delete(q.seen, detailKey(it))
		return tea.Sequence(func() tea.Msg {
			if err := pressPrompt(it, seen, key); err != nil {
				return queueStatusMsg{text: firstLine(err.Error()), color: cRed}
			}
			return queueStatusMsg{text: did + it.Key + "'s pane", color: cGreen}
		}, pollQueue)
	case "enter", "s":
		if it.Kind == prItem {
			agent, ok := q.prAgent(*it.Pr)
			if !ok {
				return q.requestForm(*it.Pr)
			}
			it = agent
		}
		if it.Agent == "" && it.Watcher {
			q.say("Starting "+it.Key+"…", cSubtext)
			return func() tea.Msg {
				if _, err := startWatchers(); err != nil {
					return queueStatusMsg{text: err.Error(), color: cRed}
				}
				return pollQueue()
			}
		}
		if it.Agent == "" {
			return q.resume(it)
		}
		session := it.Session
		return func() tea.Msg {
			if err := rex.show(session); err != nil {
				return queueStatusMsg{text: "Switching to " + it.Key + "'s session: " + err.Error(), color: cRed}
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
		case it.Watcher, it.Orchestrator:
		case isTask(it.Key):
			q.say("A task has no ticket or PR to open", cOverlay)
		case it.Pr != nil:
			openURL(it.Pr.URL)
		case it.review(), isFix(it.Key):
			if n := slices.IndexFunc(q.prs, func(p Pr) bool { return p.Key == strings.TrimPrefix(it.Key, fixPrefix) }); n >= 0 {
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
		q.feedback = &reply{key: it.Key, session: it.Session, block: it.Block, answer: it.Question != nil}
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
			if it.Watcher {
				q.say(it.Key+" isn't running; it stays listed while the config names it", cOverlay)
				return nil
			}
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
			if err := rex.kill(session); err != nil {
				return queueStatusMsg{text: "Closing " + key + "'s session: " + err.Error(), color: cRed}
			}
			return queueStatusMsg{text: "✓ Closed " + key + "'s session", color: cGreen}
		}
		// Poll right after, so the row moves to closed now rather than on the next tick.
		return tea.Sequence(kill, pollQueue)
	}
	return nil
}

// prAgent is the agent fixing the PR, else the one dispatched on its ticket, found by the Jira key in
// its title or branch.
// One I dropped from the queue isn't listed, but it is closed, so it is built from the state.
func (q *queueModel) prAgent(p Pr) (queueItem, bool) {
	key := fixKey(p.Key)
	if _, fixing := q.state.Dispatched[key]; !fixing {
		key = jiraKey(p, nil)
	}
	dir, ok := q.state.Dispatched[key]
	if key == "" || !ok {
		return queueItem{}, false
	}
	if n := slices.IndexFunc(q.all, func(it queueItem) bool { return it.Kind == agentItem && it.Key == key }); n >= 0 {
		return q.all[n], true
	}
	return queueItem{Kind: agentItem, Key: key, Title: q.state.Titles[key], Session: q.state.Sessions[key], Dir: dir}, true
}

// requestForm asks the board to open its dispatch form on the PR's ticket. A PR that names no
// ticket opens in the browser.
func (q *queueModel) requestForm(p Pr) tea.Cmd {
	key := jiraKey(p, nil)
	if key == "" {
		openURL(p.URL)
		return nil
	}
	return func() tea.Msg { return openFormMsg{key} }
}

// resume reopens a closed agent: a new Rex session in its worktree that continues its latest
// conversation, recorded as the agent's session and shown.
func (q *queueModel) resume(it queueItem) tea.Cmd {
	if !exists(it.Dir) {
		q.say(tilde(it.Dir)+" is gone; dispatch it again from the board", cOverlay)
		return nil
	}
	agent, err := dispatchedAgent(q.state, it.Key, it.Dir)
	if err != nil {
		q.say(err.Error(), cOverlay)
		return nil
	}
	q.say("Resuming "+it.Key+"…", cSubtext)
	key, dir, label := it.Key, it.Dir, sessionLabel(it.Key, it.Title)
	return func() tea.Msg {
		id, _, err := newSession(label, dir, agent, resumeCommand(agent))
		if err != nil {
			return queueStatusMsg{text: "Resuming " + key + ": " + err.Error(), color: cRed}
		}
		_, _ = updateState(func(s *State) {
			s.Sessions[key] = id
			s.Agents[key] = agent
		})
		if err := rex.show(id); err != nil {
			return queueStatusMsg{text: "Switching to " + key + "'s session: " + err.Error(), color: cRed}
		}
		return pollQueue()
	}
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

// openDiff shows the worktree's changes in a pager, suspending the queue: the agent's since its
// dispatch plus anything uncommitted, or a review's against the PR's base.
func (q *queueModel) openDiff(it queueItem) tea.Cmd {
	if !exists(it.Dir) {
		q.say(tilde(it.Dir)+" is gone", cOverlay)
		return nil
	}
	args := []string{"-C", it.Dir, "diff"}
	if from := diffFrom(it.Dir, it.review(), q.state.StartedFrom[it.Key]); from != "" {
		args = append(args, from)
	}
	return tea.ExecProcess(exec.Command("git", args...), func(err error) tea.Msg {
		if err != nil {
			return queueStatusMsg{text: "git diff: " + err.Error(), color: cRed}
		}
		return nil
	})
}

// diffFrom is the commit the board diffs dir's work from: started, the commit its dispatch
// recorded, while the repo still has it; else, for a dispatch from before they were recorded or a
// commit gone with a gc, where the branch forked from diffBase. "" diffs against the index alone.
func diffFrom(dir string, review bool, started string) string {
	if started != "" {
		if _, err := sh(dir, "git", "cat-file", "-e", started+"^{commit}"); err == nil {
			return started
		}
	}
	if base := diffBase(dir, review); base != "" {
		if mb, err := sh(dir, "git", "merge-base", base, "HEAD"); err == nil {
			return strings.TrimSpace(mb)
		}
	}
	return ""
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
	case "enter":
		r := *q.feedback
		q.feedback = nil
		text := strings.TrimSpace(r.text)
		if text == "" {
			return nil
		}
		q.say("Sending to "+r.key+"…", cSubtext)
		return func() tea.Msg {
			if r.answer {
				if err := answerQuestion(r.key, text, byMe); err != nil {
					return queueStatusMsg{text: "Answer to " + r.key + ": " + err.Error(), color: cRed}
				}
				return queueStatusMsg{text: "✓ Answered " + r.key, color: cGreen}
			}
			if err := sendToAgent(r.session, r.block, text); err != nil {
				return queueStatusMsg{text: "Reply to " + r.key + ": " + err.Error(), color: cRed}
			}
			return queueStatusMsg{text: "✓ Sent to " + r.key, color: cGreen}
		}
	default:
		if text, back, ok := editText(q.feedback.text, q.feedback.back, msg); ok {
			q.feedback.text, q.feedback.back = text, back
		}
	}
	return nil
}
