package main

// foreman queue: what needs me, one item at a time. Agents that stopped to ask something or
// finished (with their report and evidence), my PRs whose CI failed, got changes requested or
// conflict, and my approved PRs, ready to merge; then the rest of my open PRs, by what they wait
// on. It is drawn beside the board, by the app, and shares the state file with it.

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	waiting               // a PR of mine that waits on someone else: reviewers, CI, the PR under it
	idle                  // an agent with an open session and nothing new for me
	closed                // an agent whose session is closed
)

// A queueItem is one dispatched agent, or one of my open PRs.
type queueItem struct {
	Kind    itemKind
	Group   group
	Key     string
	Title   string
	Agent   string    // the agent's reported state, "" when its session is closed
	Doing   string    // what the agent says it is doing, when it reports that
	Session string    // the open session's ID, else the recorded one
	Block   string    // the agent's pane in that session, "" when it is closed
	Waits   string    // what a blocked agent says it waits for: permission, question or auth
	Ports   []rexPort // what the programs in its session listen on
	Dir     string    // the worktree
	Repo    string    // the repo it works in, by its main checkout's directory name; "" for none
	Report  string    // report.md's path, "" when the agent wrote none
	When    time.Time
	Shots   int // images and terminal captures
	Videos  int
	Pr      *Pr
	// InPr is the open PR of mine whose head is the commit the agent's worktree is on: its work is
	// there to review, so its report no longer waits on me.
	InPr string
	// News is what someone else did on a PR that I haven't marked seen, which lifts it to review.
	News string
	// Wait is what a PR waits on, in a few words, and On the PR of mine it is stacked on, "" for
	// none. Under marks a PR listed right under that one's stack, where its row drops what its
	// title repeats of the title above. Order is its place among my PRs, stacks kept together.
	Wait  string
	On    string
	Under string
	Order int
	// Watcher marks a watcher's session rather than a dispatched agent's.
	Watcher bool
	// Polled marks a watcher foreman watch wakes when its PRs change, rather than one that loops.
	Polled bool
	// At is the schedule of a watcher foreman watch wakes by the clock.
	At string
	// Unwoken marks a running watcher that nothing wakes: foreman watch, which would, isn't running.
	Unwoken bool
	// Orchestrator marks the orchestrator's session.
	Orchestrator bool
	// Question is the one the agent asked in foreman ask and I haven't answered. Left marks one
	// nothing waits on any more: its session closed, or its foreman ask is gone.
	Question *Question
	Left     bool
	// Decisions are the questions the agent went on without, oldest first.
	Decisions []Question
	// Sig is what the item showed; marking it reviewed holds until this changes.
	Sig string
	// Fold marks the row that stands for the closed agents while they are folded, and counts them.
	Fold int
}

// on puts the item on its open session: the agent's state, its pane, what it says it is doing or
// waits for, and the ports the session listens on.
func (it *queueItem) on(s rexSession) {
	it.Agent, it.Session, it.Block, it.Doing, it.Waits, it.Ports = s.State, s.ID, s.Block, s.Msg, s.Kind, s.Ports
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

// repoOf names the repo a dispatch works in, by its main checkout's directory: from the key for a
// PR, a fix or a task, else from the worktree's .git file. A worktree that is gone gives its name
// less the ticket key; a directory outside git (the orchestrator's) gives "".
func repoOf(key, dir string) string {
	if r, ok := strings.CutPrefix(key, "task:"); ok {
		if i := strings.LastIndex(r, "-"); i > 0 {
			return r[:i]
		}
	}
	if r, _, ok := strings.Cut(strings.TrimPrefix(key, "fix:"), "#"); ok {
		return r
	}
	if dir == "" {
		return ""
	}
	if main := mainCheckout(dir); main != "" {
		return filepath.Base(main)
	}
	if exists(dir) {
		return ""
	}
	base := filepath.Base(dir)
	if key != "" {
		base = strings.TrimSuffix(base, "-"+strings.ToLower(key))
	}
	return base
}

// mainCheckout is the checkout that owns dir: dir itself when its .git is a directory, else the one
// its .git file points into. "" when dir isn't a checkout. It reads files rather than run git, since
// the queue asks for every item at each poll.
func mainCheckout(dir string) string {
	dotGit := filepath.Join(dir, ".git")
	st, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return dir
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	// <main>/.git/worktrees/<name>
	worktrees := filepath.Dir(gitdir)
	if filepath.Base(worktrees) != "worktrees" {
		return ""
	}
	return filepath.Dir(filepath.Dir(worktrees))
}

// pendingQuestion is the question my answer goes to: the one the agent asked, else the oldest it
// went on without, which decision marks.
func (it queueItem) pendingQuestion() (q Question, decision, ok bool) {
	switch {
	case it.Question != nil:
		return *it.Question, false, true
	case len(it.Decisions) > 0:
		return it.Decisions[0], true, true
	}
	return Question{}, false, false
}

// pending is whether the item waits on me: it asks something or has a report to read.
func (it queueItem) pending() bool { return it.Group == needsYou || it.Group == toReview }

// urgent is whether the item is blocked on me rather than done: an agent that asks, failed, or
// put a question to me in foreman ask that I haven't answered, whether it still waits there or not.
func (it queueItem) urgent() bool {
	if it.Kind == prItem {
		return it.Group == needsYou
	}
	return it.Agent == "blocked" || it.Agent == "error" || it.Question != nil
}

// queuedPr is whether the PR waits on me: mine and blocked, or mine and approved (the board files
// an approved PR whose CI isn't running under Done).
func queuedPr(p Pr) bool {
	return p.Mine && (p.Category == "Blocked" || p.Category == "Done")
}

// A stacked is one of my PRs' place among them: stacks together, each PR after the one it is
// stacked on.
type stacked struct {
	order  int
	parent *Pr // the PR of mine whose branch it merges into, nil when its base is no PR of mine
}

// stackOrder places my PRs: a ticket's together, a stack from its bottom up.
func stackOrder(prs []Pr) map[string]stacked {
	var mine []Pr
	for _, p := range prs {
		if p.Mine {
			mine = append(mine, p)
		}
	}
	slices.SortFunc(mine, func(a, b Pr) int {
		return cmp.Or(strings.Compare(jiraKey(a, nil), jiraKey(b, nil)), a.Number-b.Number, strings.Compare(a.Key, b.Key))
	})
	parents := map[string]*Pr{}
	for n, p := range mine {
		for m, o := range mine {
			if n != m && p.Base != "" && o.Branch == p.Base && o.Repo == p.Repo {
				parents[p.Key] = &mine[m]
				break
			}
		}
	}
	out := map[string]stacked{}
	var place func(p Pr)
	place = func(p Pr) {
		if _, done := out[p.Key]; done {
			return
		}
		out[p.Key] = stacked{len(out), parents[p.Key]}
		for _, c := range mine {
			if on := parents[c.Key]; on != nil && on.Key == p.Key {
				place(c)
			}
		}
	}
	for _, p := range mine {
		if parents[p.Key] == nil {
			place(p)
		}
	}
	// Branches that merge into each other have no bottom: listed as they come.
	for _, p := range mine {
		place(p)
	}
	return out
}

// prWait is where my PR sits in the queue and what it waits on, in the few words its row has:
// me, under needs you or review, or someone else, under waiting. on is the PR it is stacked on.
func prWait(p Pr, on *Pr) (group, string) {
	switch {
	case p.CI == "failed":
		return needsYou, "✗ " + listed(p.Failing, 1)
	case p.Draft:
		return waiting, "draft"
	case p.Conflict:
		return needsYou, "conflicts with " + p.Base
	case p.Review == "changes":
		return needsYou, "changes requested"
	case p.Review == "approved" && p.CI == "running":
		return waiting, "approved, CI running"
	case p.Review == "approved" && on != nil:
		return waiting, fmt.Sprintf("approved, after #%d", on.Number)
	case p.Review == "approved":
		return toReview, "ready to merge"
	case p.Threads == 1:
		return needsYou, "1 thread to answer"
	case p.Threads > 1:
		return needsYou, fmt.Sprintf("%d threads to answer", p.Threads)
	case p.CI == "running":
		return waiting, "CI running"
	case p.Needs != "":
		// A status says which rule asks for the reviews, then what is missing, and on which commit:
		// the row has room for what is missing.
		_, needs, ok := cutLast(p.Needs, ": ")
		if !ok {
			needs = p.Needs
		}
		return waiting, onCommit.ReplaceAllString(needs, "")
	case len(p.Reviewers) == 0:
		return waiting, "no reviewer asked"
	}
	return waiting, "with " + listed(p.Reviewers, 1)
}

// onCommit is a status naming the commit it speaks of, which the PR's row has no use for.
var onCommit = regexp.MustCompile(` on [0-9a-f]{7,40}\b`)

// prHolding is the open PR of mine that holds the work in dir: its branch is the one checked out
// there, in the same repo, and its head the commit dir is on, so nothing there is still unpushed.
func prHolding(prs []Pr, dir string) string {
	branch, commit := worktreeHead(dir)
	if branch == "" || commit == "" {
		return ""
	}
	main := mainCheckout(dir)
	for _, p := range prs {
		if p.Mine && p.Branch == branch && p.Head == commit && p.Checkout != "" && filepath.Clean(p.Checkout) == main {
			return p.Key
		}
	}
	return ""
}

// worktreeHead is the branch checked out in dir and the commit it is on, "" for a detached HEAD or
// no checkout. Like mainCheckout it reads git's files: the queue asks for every agent at each poll.
func worktreeHead(dir string) (branch, commit string) {
	gitDir := filepath.Join(dir, ".git")
	st, err := os.Stat(gitDir)
	if err != nil {
		return "", ""
	}
	if !st.IsDir() {
		b, _ := os.ReadFile(gitDir)
		to, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
		if !ok {
			return "", ""
		}
		if gitDir = to; !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, to)
		}
	}
	head, _ := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: ")
	if !ok {
		return "", ""
	}
	branch = strings.TrimPrefix(ref, "refs/heads/")
	// A worktree's refs are its main checkout's.
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		if common = strings.TrimSpace(string(b)); !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
	}
	if b, err := os.ReadFile(filepath.Join(common, ref)); err == nil {
		return branch, strings.TrimSpace(string(b))
	}
	packed, _ := os.ReadFile(filepath.Join(common, "packed-refs"))
	for _, l := range strings.Split(string(packed), "\n") {
		if sha, name, ok := strings.Cut(l, " "); ok && name == ref {
			return branch, sha
		}
	}
	return branch, ""
}

// cutLast cuts s around the last sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// listed is the first n of names, and how many more there are.
func listed(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(names[:n], ", "), len(names)-n)
}

// sharedLead is how much of title repeats above's start, cut back to a word's start: a stack's
// titles differ in their ends. Under 12 bytes isn't worth dropping.
func sharedLead(above, title string) int {
	n := 0
	for n < len(above) && n < len(title) && above[n] == title[n] {
		n++
	}
	for n > 0 && n < len(title) && (!utf8.RuneStart(title[n]) || title[n-1] != ' ') {
		n--
	}
	if n < 12 || n >= len(title) {
		return 0
	}
	return n
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
// dispatched agent, whatever its state, and every open PR of mine, by what it waits on. Within a
// group, by repo, then newest report first, and my PRs by ticket and stack.
func queueItems(s State, sessions []rexSession, prs []Pr) []queueItem {
	var items []queueItem
	for key, d := range s.Dispatched {
		it := queueItem{Kind: agentItem, Key: key, Title: d.Title, Session: d.Session, Dir: d.Dir}
		if rs, ok := findSession(sessions, key, d.Session); ok {
			it.on(rs)
		} else if d.Hidden {
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
			it.Sig += fmt.Sprintf(" q%d", q.At.Unix())
			// Nothing waits on a question whose session closed or whose foreman ask is gone: it
			// stays mine to answer, and the answer goes to the agent as a message.
			if it.Left = it.Agent == "" || (q.Pid != 0 && !askRuns(q.Pid)); it.Left {
				it.Sig += " left"
			}
		}
		it.Decisions = s.Decisions[key]
		reviewed := s.Dismissed[key] == it.Sig
		if !it.review() {
			it.InPr = prHolding(prs, d.Dir)
		}
		switch {
		case it.urgent() && !reviewed:
			it.Group = needsYou
		// What it went on with is mine to confirm, while it works on.
		case len(it.Decisions) > 0:
			it.Group = toReview
		case it.Agent == "working":
			it.Group = working
		case it.Report != "" && !reviewed && it.InPr == "":
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
	stacks := stackOrder(prs)
	for _, p := range prs {
		if !p.Mine {
			continue
		}
		st := stacks[p.Key]
		it := queueItem{Kind: prItem, Key: p.Key, Title: p.Summary, Pr: &p, When: p.Since, Order: st.order}
		it.Group, it.Wait = prWait(p, st.parent)
		if st.parent != nil {
			it.On = st.parent.Key
		}
		it.Sig = p.Status + " " + it.Wait
		if p.News != "" {
			it.Sig += " " + p.NewsAt.UTC().Format(time.RFC3339)
		}
		switch reviewed := s.Dismissed[prDismissKey(p.Key)] == it.Sig; {
		// Marked reviewed, it waits with the rest until what it waits on changes or someone writes on it.
		case reviewed:
			it.Group = waiting
		// What someone did on a PR that waits on others is mine to read.
		case it.Group == waiting && p.News != "":
			it.Group, it.News = toReview, "new: "+p.News
		}
		items = append(items, it)
	}
	// Within a group, a repo's items together, the repo with the newest item first.
	type run struct {
		g    group
		repo string
	}
	newest := map[run]time.Time{}
	for i := range items {
		it := &items[i]
		it.Repo = repoOf(it.Key, it.Dir)
		if r := (run{it.Group, it.Repo}); it.When.After(newest[r]) {
			newest[r] = it.When
		}
	}
	slices.SortFunc(items, func(a, b queueItem) int {
		if d := int(a.Group) - int(b.Group); d != 0 {
			return d
		}
		if d := newest[run{b.Group, b.Repo}].Compare(newest[run{a.Group, a.Repo}]); d != 0 {
			return d
		}
		if d := strings.Compare(a.Repo, b.Repo); d != 0 {
			return d
		}
		// Agents that ask before my PRs.
		if d := int(a.Kind) - int(b.Kind); d != 0 {
			return d
		}
		if d := a.Order - b.Order; d != 0 {
			return d
		}
		if d := b.When.Compare(a.When); d != 0 {
			return d
		}
		return strings.Compare(a.Key, b.Key)
	})
	// A PR right under its stack's rows says only what its title adds to the one it is stacked on.
	for n := range items {
		it := &items[n]
		if it.On == "" || n == 0 {
			continue
		}
		for m := n - 1; m >= 0 && items[m].Kind == prItem && items[m].Group == it.Group && items[m].Repo == it.Repo; m-- {
			if items[m].Key == it.On {
				it.Under = items[m].Title
				break
			}
		}
	}
	return items
}

// sessionFixes are the keys whose open session is no longer the one recorded, by ID: a label that
// the agent renamed, or a dispatch from before IDs were kept. Recording the ID lets the board find it too.
func sessionFixes(s State, sessions []rexSession) map[string]string {
	fixes := map[string]string{}
	for key, d := range s.Dispatched {
		if rs, ok := findSession(sessions, key, d.Session); ok && rs.ID != d.Session {
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
		watching bool // whether a foreman watch runs
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
	// closeQueueMsg asks the app to put the queue away and show the board alone.
	closeQueueMsg struct{}
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
	watching      bool // whether a foreman watch runs, as of the last poll
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
	portNext    map[string]int      // by key: which of an agent's ports p opens next
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
	back                      int       // the text's cursor, in runes from its end
	answer                    bool      // the text answers the agent's pending question
	asked                     time.Time // when that question was asked: the answer is for it alone
}

// detailKey is what an item's detail is kept under. The questions it went on without head the
// detail but aren't in Sig: confirming them mustn't bring a report marked reviewed back.
func detailKey(it queueItem) string {
	key := it.Key + " " + it.Sig
	if n := len(it.Decisions); n > 0 {
		key += fmt.Sprintf(" d%d-%d", n, it.Decisions[0].At.Unix())
	}
	return key
}

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
	return queueStateMsg{loadState(), sessions, err == nil, watchRuns()}
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
	for n, it := range q.all {
		q.all[n].Unwoken = !q.watching && it.Watcher && (it.Polled || it.At != "") && it.Agent != ""
	}
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

// prWaitLines start a PR's detail with what it waits on in full: what its row says, the review
// check's own words, who is asked to review, and the PR it is stacked on.
func prWaitLines(it queueItem) []string {
	p := it.Pr
	var out []string
	if p.News != "" {
		when := age(p.NewsAt) + " ago"
		if when == "now ago" {
			when = "just now"
		}
		out = append(out, "**Latest from others:** "+p.News+", "+when, "")
	}
	parts := []string{it.Wait}
	if p.Needs != "" && it.Group == waiting && !p.Draft {
		parts = []string{p.Needs}
	}
	if len(p.Reviewers) > 0 {
		parts = append(parts, "review asked of "+strings.Join(p.Reviewers, ", "))
	}
	if it.On != "" {
		parts = append(parts, "stacked on "+it.On)
	}
	return append(out, "**Waits on:** "+strings.Join(parts, " · "), "")
}

// itemDetail is the item's report, its evidence and terminal captures, or for an agent that wrote
// no report, the end of its screen, where its question is.
func itemDetail(it queueItem) []string {
	if it.Kind == prItem {
		v, err := viewPr(*it.Pr)
		if err != nil {
			return []string{fg(cRed, "Failed to load the PR: "+err.Error())}
		}
		return append(prWaitLines(it), strings.Split(prMarkdown(*it.Pr, v), "\n")...)
	}
	out := questionLines(it)
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
		q.state, q.watching = msg.state, msg.watching
		if msg.ok {
			q.sessions = msg.sessions
			if fixes := sessionFixes(q.state, q.sessions); len(fixes) > 0 {
				q.state, _ = updateState(func(s *State) {
					for k, id := range fixes {
						s.change(k, func(d *Dispatched) { d.Session = id })
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
	case "ctrl+c":
		return tea.Quit
	// q closes the queue, not foreman: the board stays. No esc: it is easy to press.
	case "q":
		return func() tea.Msg { return closeQueueMsg{} }
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
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "a":
		return q.choose(it, msg.String())
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
			name := it.Key
			return func() tea.Msg {
				if err := restartWatcher(name); err != nil {
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
		media, captures := evidence(evidenceDir(it.Key))
		if len(media) == 0 && len(captures) == 0 {
			q.say("No screenshots or video for "+it.Key, cOverlay)
			return nil
		}
		key, title := it.Key, it.Title
		return func() tea.Msg { return showEvidence(key, title, media) }
	case "d":
		if it.Kind == agentItem {
			return q.openDiff(it)
		}
	case "p":
		if len(it.Ports) == 0 {
			q.say("Nothing in "+it.Key+"'s session listens on a port", cOverlay)
			return nil
		}
		// One port a press, in turn: which of several is the app, only I can tell.
		n := q.portNext[it.Key] % len(it.Ports)
		if q.portNext == nil {
			q.portNext = map[string]int{}
		}
		q.portNext[it.Key] = n + 1
		said := "Opened " + it.Ports[n].url()
		if len(it.Ports) > 1 {
			said += fmt.Sprintf("; p again opens :%d", it.Ports[(n+1)%len(it.Ports)].Port)
		}
		q.say(said, cOverlay)
		return q.browse(it.Ports[n].url())
	case "o", "O":
		url := ""
		switch {
		case it.Watcher, it.Orchestrator:
		case isTask(it.Key):
			q.say("A task has no ticket or PR to open", cOverlay)
		case it.Pr != nil:
			url = it.Pr.URL
		case it.review(), isFix(it.Key):
			if n := slices.IndexFunc(q.prs, func(p Pr) bool { return p.Key == strings.TrimPrefix(it.Key, fixPrefix) }); n >= 0 {
				url = q.prs[n].URL
			}
		default:
			url = site + "/browse/" + it.Key
		}
		if url == "" {
			return nil
		}
		var show tea.Cmd
		if msg.String() == "O" {
			openURL(url)
		} else {
			show = q.browse(url)
		}
		// Opened, what someone did on it is read.
		if it.Pr != nil && it.News != "" {
			return tea.Batch(show, q.markReviewed(it))
		}
		return show
	case "f":
		// A question outlives its session: answering it resumes the agent.
		asked, _, asks := it.pendingQuestion()
		if it.Kind == prItem || (it.Agent == "" && !asks) {
			q.say("No open agent session to reply to", cOverlay)
			return nil
		}
		q.feedback = &reply{key: it.Key, session: it.Session, block: it.Block, answer: asks, asked: asked.At}
	case "x":
		if !it.pending() {
			q.say(it.Key+" has nothing waiting for you", cOverlay)
			return nil
		}
		return q.markReviewed(it)
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
		session, key, watcher := it.Session, it.Key, it.Watcher
		q.say("Closing "+key+"'s session…", cSubtext)
		kill := func() tea.Msg {
			if err := closeSession(session, key, watcher); err != nil {
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
	d, ok := q.state.Dispatched[key]
	if key == "" || !ok {
		return queueItem{}, false
	}
	if n := slices.IndexFunc(q.all, func(it queueItem) bool { return it.Kind == agentItem && it.Key == key }); n >= 0 {
		return q.all[n], true
	}
	return queueItem{Kind: agentItem, Key: key, Title: d.Title, Session: d.Session, Dir: d.Dir}, true
}

// requestForm asks the board to open its dispatch form on the PR's ticket. A PR that names no
// ticket opens in the browser.
func (q *queueModel) requestForm(p Pr) tea.Cmd {
	key := jiraKey(p, nil)
	if key == "" {
		return q.browse(p.URL)
	}
	return func() tea.Msg { return openFormMsg{key} }
}

// browse opens the URL beside the board, and says so when it can't.
func (q *queueModel) browse(url string) tea.Cmd {
	return func() tea.Msg {
		if err := browse(url); err != nil {
			return queueStatusMsg{text: "Opening " + url + ": " + firstLine(err.Error()), color: cRed}
		}
		return nil
	}
}

// choose answers the item's pending question with one of its choices, by its digit, or with a, the
// one the agent recommends.
func (q *queueModel) choose(it queueItem, key string) tea.Cmd {
	asked, _, ok := it.pendingQuestion()
	if !ok {
		q.say(it.Key+" asks nothing to choose from", cOverlay)
		return nil
	}
	answer := asked.Recommended
	if key != "a" {
		n := int(key[0] - '0')
		if n > len(asked.Options) {
			q.say(fmt.Sprintf("%s's question has %d choices; f answers in your own words", it.Key, len(asked.Options)), cOverlay)
			return nil
		}
		answer = asked.Options[n-1]
	}
	if answer == "" {
		q.say(it.Key+" recommends nothing; f answers in your own words", cOverlay)
		return nil
	}
	state := q.state
	q.say("Answering "+it.Key+"…", cSubtext)
	return tea.Sequence(func() tea.Msg {
		said, err := answerItem(state, it, answer, byMe)
		if err != nil {
			return queueStatusMsg{text: "Answer to " + it.Key + ": " + firstLine(err.Error()), color: cRed}
		}
		return queueStatusMsg{text: said, color: cGreen}
	}, pollQueue)
}

// markReviewed takes the item out of what waits on me until it has something new: a report, or
// for a PR, something else to wait on or something someone wrote on it. What an agent went on
// with is confirmed.
func (q *queueModel) markReviewed(it queueItem) tea.Cmd {
	key, dkey, sig := it.Key, it.dismissKey(), it.Sig
	q.state, _ = updateState(func(s *State) { s.Dismissed[dkey] = sig })
	if it.Kind == agentItem && len(it.Decisions) > 0 {
		if s, err := confirm(key, it.Decisions, byMe); err == nil {
			q.state = s
		}
	}
	if it.Kind == prItem {
		q.say("Marked "+key+" reviewed; it comes back when what it waits on changes or someone writes on it", cOverlay)
	} else {
		q.say("Marked "+key+" reviewed; it moves up again with a new report", cOverlay)
	}
	return q.rebuild()
}

// resumeAgent reopens a closed agent: a new Rex session in its worktree that continues its latest
// conversation, recorded as the agent's session. With a message, the agent starts on it.
func resumeAgent(s State, key, title, dir, message string) (id string, err error) {
	if !exists(dir) {
		return "", fmt.Errorf("%s is gone; dispatch it again from the board", tilde(dir))
	}
	agent, err := dispatchedAgent(s, key, dir)
	if err != nil {
		return "", err
	}
	id, _, err = newSession(sessionLabel(key, title), dir, agent, resumeCommand(agent, message))
	if err != nil {
		return "", err
	}
	_, _ = updateState(func(s *State) {
		s.change(key, func(d *Dispatched) { d.Session, d.Agent = id, agent })
	})
	return id, nil
}

// resume reopens a closed agent from its row, and shows its session.
func (q *queueModel) resume(it queueItem) tea.Cmd {
	if !exists(it.Dir) {
		q.say(tilde(it.Dir)+" is gone; dispatch it again from the board", cOverlay)
		return nil
	}
	if _, err := dispatchedAgent(q.state, it.Key, it.Dir); err != nil {
		q.say(err.Error(), cOverlay)
		return nil
	}
	q.say("Resuming "+it.Key+"…", cSubtext)
	state, key, title, dir := q.state, it.Key, it.Title, it.Dir
	return func() tea.Msg {
		id, err := resumeAgent(state, key, title, dir, "")
		if err != nil {
			return queueStatusMsg{text: "Resuming " + key + ": " + err.Error(), color: cRed}
		}
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
	q.state, _ = updateState(func(s *State) { s.change(key, func(d *Dispatched) { d.Hidden = true }) })
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
	if from := diffFrom(it.Dir, it.review(), q.state.Dispatched[it.Key].StartedFrom); from != "" {
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
		if r.answer {
			// The list was rebuilt while I typed: the answer goes only to the question it was for.
			n := slices.IndexFunc(q.all, func(it queueItem) bool { return it.Kind == agentItem && it.Key == r.key })
			if n < 0 {
				q.say(r.key+" is gone from the queue; nothing was sent", cYellow)
				return nil
			}
			it := q.all[n]
			if asked, _, ok := it.pendingQuestion(); !ok || !asked.At.Equal(r.asked) {
				q.say(r.key+" no longer asks what you answered; nothing was sent", cYellow)
				return nil
			}
			state := q.state
			q.say("Answering "+r.key+"…", cSubtext)
			return tea.Sequence(func() tea.Msg {
				said, err := answerItem(state, it, text, byMe)
				if err != nil {
					return queueStatusMsg{text: "Answer to " + r.key + ": " + firstLine(err.Error()), color: cRed}
				}
				return queueStatusMsg{text: said, color: cGreen}
			}, pollQueue)
		}
		q.say("Sending to "+r.key+"…", cSubtext)
		return func() tea.Msg {
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
