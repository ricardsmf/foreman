package main

// Reviewing an agent's work before any of it is in a PR: the queue's r shows the diff since its
// dispatch, where I comment on lines and send the comments to the agent as one reply, and
// foreman dispatch --review puts another agent on it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A diffLine is a line of a diff as git prints it, and where it is in the change: its file and,
// in a hunk, its number in the new file, or for a removed line in the old one.
type diffLine struct {
	text    string
	file    string
	line    int
	removed bool
	hunk    bool // a line of a hunk, which a comment can be on
}

var hunkHead = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseDiff reads git's unified diff. A hunk's lines are counted off its header, so a line of
// content that looks like a header ("--- a") stays content.
func parseDiff(out string) []diffLine {
	var lines []diffLine
	file := ""
	oldAt, newAt, oldLeft, newLeft := 0, 0, 0, 0
	for _, text := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		l := diffLine{text: text, file: file}
		switch {
		case oldLeft > 0 || newLeft > 0:
			l.hunk = true
			switch {
			case strings.HasPrefix(text, "+"):
				l.line = newAt
				newAt, newLeft = newAt+1, newLeft-1
			case strings.HasPrefix(text, "-"):
				l.line, l.removed = oldAt, true
				oldAt, oldLeft = oldAt+1, oldLeft-1
			case strings.HasPrefix(text, "\\"):
				l.hunk = false
			default:
				l.line = newAt
				oldAt, newAt, oldLeft, newLeft = oldAt+1, newAt+1, oldLeft-1, newLeft-1
			}
		case strings.HasPrefix(text, "diff --git "):
			file, l.file = "", ""
		case strings.HasPrefix(text, "--- a/") && file == "":
			// Until +++ names the new file: a deleted one has only this name.
			file = strings.TrimPrefix(text, "--- a/")
			l.file = file
		case strings.HasPrefix(text, "+++ b/"):
			file = strings.TrimPrefix(text, "+++ b/")
			l.file = file
		default:
			if m := hunkHead.FindStringSubmatch(text); m != nil {
				oldAt, oldLeft = hunkRange(m[1], m[2])
				newAt, newLeft = hunkRange(m[3], m[4])
			}
		}
		lines = append(lines, l)
	}
	return lines
}

// hunkRange is a hunk header's start and count, the count being 1 when it is left out.
func hunkRange(start, count string) (int, int) {
	s, _ := strconv.Atoi(start)
	n := 1
	if count != "" {
		n, _ = strconv.Atoi(count)
	}
	return s, n
}

// where names the line for the agent: its file and number.
func (l diffLine) where() string {
	if l.removed {
		return fmt.Sprintf("%s, removed line %d", l.file, l.line)
	}
	return fmt.Sprintf("%s:%d", l.file, l.line)
}

// A diffReview is the queue's view of an agent's diff, with the comments I put on its lines.
type diffReview struct {
	key      string
	from     string // the commit the diff starts from, "" for the index
	loading  bool
	lines    []diffLine
	cursor   int
	top      int
	comments map[int]string // by the line's place in lines
	typing   bool           // a comment on the cursor's line is being typed
	text     string
	back     int
	leaving  bool // esc was pressed once with comments unsent
}

type queueDiffMsg struct {
	key, from, out string
	err            error
}

// openReview shows the diff of the agent's work since its dispatch, as d does in git's pager.
func (q *queueModel) openReview(it queueItem) tea.Cmd {
	switch {
	case it.Kind != agentItem || it.Watcher || it.Orchestrator || it.review():
		q.say("r comments on the work of a ticket's, a task's or a fix's agent", cOverlay)
		return nil
	case !exists(it.Dir):
		q.say(tilde(it.Dir)+" is gone", cOverlay)
		return nil
	}
	q.review = &diffReview{key: it.Key, loading: true, comments: map[int]string{}}
	key, dir, started := it.Key, it.Dir, q.state.Dispatched[it.Key].StartedFrom
	return func() tea.Msg {
		from := diffFrom(dir, false, started)
		args := []string{"diff", "--no-color"}
		if from != "" {
			args = append(args, from)
		}
		out, err := sh(dir, "git", args...)
		return queueDiffMsg{key, from, out, err}
	}
}

// takeDiff puts a loaded diff in the view that asked for it, the cursor on its first line of a hunk.
func (q *queueModel) takeDiff(msg queueDiffMsg) {
	r := q.review
	if r == nil || r.key != msg.key || !r.loading {
		return
	}
	if msg.err != nil || strings.TrimSpace(msg.out) == "" {
		q.review = nil
		if msg.err != nil {
			q.say("git diff: "+firstLine(msg.err.Error()), cRed)
		} else {
			q.say(msg.key+" has changed nothing since it was dispatched", cOverlay)
		}
		return
	}
	r.loading, r.from, r.lines = false, msg.from, parseDiff(msg.out)
	r.cursor = max(0, slices.IndexFunc(r.lines, func(l diffLine) bool { return l.hunk }))
}

// reviewInput takes a key on the diff: moving, commenting on the line under the cursor, sending.
func (q *queueModel) reviewInput(msg tea.KeyPressMsg) tea.Cmd {
	r := q.review
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if r.typing {
		switch key {
		case "esc":
			r.typing = false
		case "enter":
			r.typing = false
			if text := strings.TrimSpace(r.text); text != "" {
				r.comments[r.cursor] = text
			} else {
				delete(r.comments, r.cursor)
			}
		default:
			if text, back, ok := editText(r.text, r.back, msg); ok {
				r.text, r.back = text, back
			}
		}
		return nil
	}
	if key != "esc" && key != "q" {
		r.leaving = false
	}
	move := func(to int) { r.cursor = max(0, min(to, len(r.lines)-1)) }
	// hunk moves to the next line that starts a hunk, forwards or back.
	hunk := func(step int) {
		for n := r.cursor + step; n >= 0 && n < len(r.lines); n += step {
			if strings.HasPrefix(r.lines[n].text, "@@") && !r.lines[n].hunk {
				move(n + 1)
				return
			}
		}
	}
	switch key {
	case "esc", "q":
		if len(r.comments) > 0 && !r.leaving {
			r.leaving = true
			q.say(fmt.Sprintf("%s unsent: s sends them, %s again drops them", plural(len(r.comments), "comment"), key), cYellow)
			return nil
		}
		q.review = nil
	case "up", "k", "ctrl+p":
		move(r.cursor - 1)
	case "down", "j", "ctrl+n":
		move(r.cursor + 1)
	case "ctrl+u", "pgup":
		move(r.cursor - 10)
	case "ctrl+d", "pgdown", "space":
		move(r.cursor + 10)
	case "g", "home":
		move(0)
	case "G", "end":
		move(len(r.lines) - 1)
	case "n":
		hunk(1)
	case "p":
		hunk(-1)
	case "c", "enter":
		if r.loading || len(r.lines) == 0 {
			return nil
		}
		if !r.lines[r.cursor].hunk {
			q.say("A comment goes on a line of the change", cOverlay)
			return nil
		}
		r.typing, r.text, r.back = true, r.comments[r.cursor], 0
	case "x":
		delete(r.comments, r.cursor)
	case "s":
		return q.sendReview()
	}
	return nil
}

// reviewText is the comments as the agent reads them, in the diff's order: each with the file
// and line it is on, and the line itself.
func reviewText(key string, r *diffReview) string {
	out := []string{"# Review of " + key + "'s changes", "",
		plural(len(r.comments), "comment") + " on the diff of your work since it started. Each names the file and line it is on, then shows the line."}
	n := 0
	for at, l := range r.lines {
		c, ok := r.comments[at]
		if !ok {
			continue
		}
		n++
		out = append(out, "", fmt.Sprintf("## %d. %s", n, l.where()), "", "    "+l.text, "", c)
	}
	return strings.Join(out, "\n") + "\n"
}

// sendReview writes the comments beside the agent's report and tells the agent to read them:
// typed into its pane, or for a closed session, as the turn the resumed agent starts with.
func (q *queueModel) sendReview() tea.Cmd {
	r := q.review
	if len(r.comments) == 0 {
		q.say("No comment yet: c puts one on the line under the cursor", cOverlay)
		return nil
	}
	n := slices.IndexFunc(q.all, func(it queueItem) bool { return it.Kind == agentItem && it.Key == r.key })
	if n < 0 {
		q.say(r.key+" is gone from the queue; nothing was sent", cYellow)
		return nil
	}
	it, state, text, count := q.all[n], q.state, reviewText(r.key, r), plural(len(r.comments), "comment")
	q.review = nil
	q.say("Sending "+it.Key+" "+count+"…", cSubtext)
	return tea.Sequence(func() tea.Msg {
		said, err := deliverReview(state, it, text, count)
		if err != nil {
			return queueStatusMsg{text: "Review of " + it.Key + ": " + firstLine(err.Error()), color: cRed}
		}
		return queueStatusMsg{text: said, color: cGreen}
	}, pollQueue)
}

// reviewFile is where my comments on a dispatch's diff are kept for its agent to read.
func reviewFile(key string) string { return filepath.Join(evidenceDir(key), "review.md") }

func deliverReview(s State, it queueItem, text, count string) (string, error) {
	file := reviewFile(it.Key)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		return "", err
	}
	message := fmt.Sprintf("I reviewed your changes and left %s on lines of the diff, in %s. Read it and address each one.", count, file)
	said := "✓ Sent " + it.Key + " " + count + " in its pane"
	if it.Agent == "" {
		if _, err := resumeAgent(s, it.Key, it.Title, it.Dir, message); err != nil {
			return "", err
		}
		said = "✓ Resumed " + it.Key + " with " + count
	} else if err := sendToAgent(it.Session, it.Block, message); err != nil {
		return "", err
	}
	_ = appendLog(it.Key, logEntry{By: byMe, Kind: "review", Text: count + " on its diff"})
	return said, nil
}

func (q *queueModel) renderReview(width, h int) []string {
	r := q.review
	dot := fg(cSurface, " · ")
	head := bold(fg(cMauve, "review")) + dot + fg(cText, r.key)
	if r.from != "" {
		head += dot + fg(cSubtext, "since "+short(r.from))
	}
	if n := len(r.comments); n > 0 {
		head += dot + fg(cYellow, plural(n, "comment"))
	}
	keys := "↑↓ line · n p hunk · c comment · x drop it · s send them to the agent · esc back"
	if r.typing {
		keys = "type the comment · enter keep it · esc cancel"
	}
	footer := hints(keys, width)
	var box []string
	if r.typing {
		box = append(box, fg(cSurface, strings.Repeat("─", width)), bold(fg(cMauve, "Comment on "+r.lines[r.cursor].where())))
		for _, l := range wrap(cursorText(r.text, r.back), width-2) {
			box = append(box, "  "+l)
		}
	}
	rows := max(1, h-1-len(box)-len(footer))
	out := []string{head}
	if r.loading {
		out = append(out, fg(cOverlay, "  Loading the diff…"))
	}
	// A line with a comment takes more rows than one, so the top moves until the cursor's fits.
	var body []string
	r.top = min(r.top, r.cursor)
	for {
		body = body[:0]
		for n := r.top; n < len(r.lines) && len(body) < rows; n++ {
			body = append(body, r.lineRows(n, width)...)
			if n == r.cursor && len(body) > rows && r.top < r.cursor {
				break
			}
		}
		through := 0
		for n := r.top; n <= r.cursor && n < len(r.lines); n++ {
			through += len(r.lineRows(n, width))
		}
		if through <= rows || r.top >= r.cursor {
			break
		}
		r.top++
	}
	body = body[:min(len(body), rows)]
	out = append(out, body...)
	for len(out) < 1+rows {
		out = append(out, "")
	}
	return append(append(out, box...), footer...)
}

// lineRows are the rows a line of the diff takes: itself, coloured by what it does, and under
// it the comment on it.
func (r *diffReview) lineRows(n, width int) []string {
	l := r.lines[n]
	text := strings.ReplaceAll(l.text, "\t", "    ")
	number := "     "
	c := cOverlay
	switch {
	case !l.hunk && strings.HasPrefix(l.text, "@@"):
		c = cBlue
	case !l.hunk:
		text = bold(text)
	case strings.HasPrefix(l.text, "+"):
		number, c = fmt.Sprintf("%4d ", l.line), cGreen
	case l.removed:
		c = cRed
	default:
		number, c = fmt.Sprintf("%4d ", l.line), cSubtext
	}
	mark := " "
	if _, ok := r.comments[n]; ok {
		mark = fg(cYellow, "●")
	}
	row := mark + fg(cOverlay, number) + fg(c, ansi.Truncate(text, max(0, width-7), "…"))
	if n == r.cursor {
		row = selected(row, width)
	}
	rows := []string{row}
	if comment, ok := r.comments[n]; ok {
		for _, w := range wrap(comment, max(10, width-10)) {
			rows = append(rows, "       "+fg(cYellow, "↳ "+w))
		}
	}
	return rows
}

// A WorkReview asks for a review of what a dispatch's agent did, before any of it is pushed.
type WorkReview struct {
	Key     string // the dispatch whose work is reviewed
	Agent   string
	Context string
}

// dispatchWorkReview starts a read-only review of the commits the agent on Key made since its
// dispatch. The review is a task of the same repo, in a worktree of its own detached at the
// head of that work: it stays apart from the agent's conversation, which resuming it picks by
// directory, and lasts until its session is closed, as a task does.
func dispatchWorkReview(ctx context.Context, jira *Jira, r WorkReview, progress func(string)) (string, error) {
	d := loadState().Dispatched[r.Key]
	repo := workRepo(d.Dir)
	switch {
	case reviewKey(r.Key):
		return "", fmt.Errorf("%s is a review already; --review takes the work of a ticket's, a task's or a fix's agent", r.Key)
	case d.Dir == "" || repo == "":
		return "", fmt.Errorf("%s's worktree %s is gone", r.Key, tilde(d.Dir))
	}
	head, from := headCommit(d.Dir), diffFrom(d.Dir, false, d.StartedFrom)
	if head == "" || from == "" {
		return "", fmt.Errorf("can't tell where %s's work starts in %s", r.Key, tilde(d.Dir))
	}
	count, err := sh(d.Dir, "git", "rev-list", "--count", from+"..HEAD")
	if err != nil {
		return "", err
	}
	commits, _ := strconv.Atoi(strings.TrimSpace(count))
	if commits == 0 {
		return "", fmt.Errorf("%s has no commit since it was dispatched: a review reads what is committed", r.Key)
	}
	key, n, err := newTaskKey(repo)
	if err != nil {
		return "", err
	}
	cwd := fmt.Sprintf("%s-task-%d", repo, n)
	if err := forgetGone(repo); err != nil {
		return "", err
	}
	if _, err := sh("", "git", "-C", repo, "worktree", "add", "--detach", cwd, head); err != nil {
		return "", err
	}
	progress(fmt.Sprintf("Created worktree %s at %s", tilde(cwd), short(head)))
	ticket := ""
	if isTicket(r.Key) {
		progress(fmt.Sprintf("Reading %s…", r.Key))
		ticket, _ = jira.detail(ctx, r.Key)
	}
	title := taskTitle("Review " + r.Key + ": " + d.Title)
	if err := resetEvidence(key); err != nil {
		return "", err
	}
	prompt := workReviewPrompt(key, title, r, cwd, head, from, commits, ticket)
	session, name, err := start(r.Agent, key, prompt, cwd, sessionLabel(key, title), progress)
	if err != nil {
		return "", err
	}
	// A review changes no code, so it has no work to diff from a start.
	markDispatched(key, title, cwd, session, r.Agent, "")
	return fmt.Sprintf("✓ %s is reviewing %s's work as %s in session %s · %s", r.Agent, r.Key, key, name, tilde(cwd)), nil
}

// workReviewPrompt is what the reviewer starts with: the diff to read, what the agent was asked
// and what it reports, where its evidence still has them.
func workReviewPrompt(key, title string, r WorkReview, cwd, head, from string, commits int, ticket string) string {
	lines := []string{
		fmt.Sprintf("# %s: %q", key, title),
		"",
		fmt.Sprintf("You are reviewing what an agent did on %s, before any of it is pushed. Do not change it.", r.Key),
		"Working directory: " + cwd,
		fmt.Sprintf("Checked out: the head of its work %s (detached), a git worktree created for this review.", head),
		fmt.Sprintf("Diff to review: `git diff %s..HEAD`, the %s it made since it was dispatched.", short(from), plural(commits, "commit")),
	}
	asked, report := promptFile(r.Key), filepath.Join(evidenceDir(r.Key), "report.md")
	if exists(asked) {
		lines = append(lines, "What it was asked: `"+asked+"`.")
	}
	if exists(report) {
		lines = append(lines, "What it says it did: `"+report+"`.")
	}
	if ticket != "" {
		lines = append(lines, "", "## Jira ticket", "", ticket)
	}
	if r.Context != "" {
		lines = append(lines, "", "## Extra context from me", "", r.Context)
	}
	lines = append(lines, skillsSection(r.Agent, cwd)...)
	lines = append(lines,
		"",
		"## Workflow",
		"",
		"1. **Understand.** Read the repo's AGENTS.md / CLAUDE.md, what the agent was asked and its report, and the code around each change.",
		"2. **Review** the diff for correctness, missed cases, regressions, security, tests, and fit with the codebase's conventions. Check it against what was asked, and each claim of the report against the code.",
		"3. **Verify** where it helps: run the tests and linters that cover the changed code. Don't edit tracked files.",
		"4. **Report**: a verdict, then findings ordered by severity, each with `file:line`, what is wrong and a suggested fix. Separate blocking issues from nits. Write it as described below and end your final message with it.",
		"",
		"Do not commit, push, or message the agent. I'll pass on what matters myself.",
	)
	lines = append(lines, reportSection(key, false)...)
	return strings.Join(lines, "\n")
}

// workRepo is the repo a dispatch's directory belongs to, as a dispatch names it: git's own path
// for the main checkout may have gone through a symlink. "" when dir is no checkout.
func workRepo(dir string) string {
	repo := mainCheckout(dir)
	for _, known := range listRepos() {
		if repo != "" && samePath(known, repo) {
			return known
		}
	}
	return repo
}

// reviewedDispatch is the dispatch --review names, in any case.
func reviewedDispatch(s State, arg string) (string, error) {
	for key := range s.Dispatched {
		if strings.EqualFold(key, arg) {
			return key, nil
		}
	}
	return "", errors.New("no dispatch " + arg + "; foreman agents lists them")
}
