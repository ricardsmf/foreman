// foreman [PROJECT | JQL]: Jira board with assignees over a detail pane; Enter opens a
// dispatch form that starts pi or Claude Code on the ticket in a git worktree, in a new Rex session.
// Tab switches to open PRs in the repos under ~/work/code (mine with review and CI status), where Enter dispatches a review.
package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

var projectRe = regexp.MustCompile(`(?i)^[A-Z][A-Z0-9]+$`)

func parseArgs(argv []string) (jql, title string) {
	arg := strings.TrimSpace(strings.Join(argv, " "))
	if arg != "" && !projectRe.MatchString(arg) {
		return arg, "custom JQL"
	}
	project := strings.ToUpper(cmp.Or(arg, defaultProject))
	return fmt.Sprintf("project = %s AND statusCategory != Done ORDER BY updated DESC", project), project + " open tickets"
}

func openURL(url string) { _ = exec.Command("open", url).Start() }

// Row is a ticket or a PR in the board's common shape.
type Row struct {
	Key, Summary, Status, Category, Assignee string
	Pr                                       *Pr
	Issue                                    *Issue
}

// isMine is whether the row is my ticket or my PR.
func (r Row) isMine() bool {
	if r.Pr != nil {
		return r.Pr.Mine
	}
	return r.Issue != nil && r.Issue.Mine
}

func issueRow(i Issue) Row {
	return Row{i.Key, i.Summary, i.Status, i.Category, i.Assignee, nil, &i}
}

func prRow(p Pr) Row {
	return Row{p.Key, p.Summary, p.Status, p.Category, p.Assignee, &p, nil}
}

type viewKind int

const (
	tickets viewKind = iota
	prs
)

// How long a background call may take before the board gives up on it. Jira searches page
// through up to 500 tickets; a PR load runs a git command per repo and up to five GraphQL pages.
const (
	jiraTimeout     = 90 * time.Second
	refreshInterval = 5 * time.Minute
	agentsInterval  = 5 * time.Second
	// A dispatch fetches, creates a worktree and starts an agent; the browser sign-in alone may take minutes.
	dispatchTimeout = 10 * time.Minute
)

// The dispatch form's fields, in tab order. A review has only the agent and the context.
const (
	fieldAgent = iota
	fieldWorktree
	fieldPoteto
	fieldRepo
	fieldContext
	fieldCount
)

type form struct {
	row Row
	// task is a dispatch with no ticket or PR: the context field holds the task itself.
	task  bool
	field int
	agent int
	// agentPicked is set once I choose the agent by hand; until then it follows the repo.
	agentPicked bool
	worktree    bool
	poteto      bool
	repo        int
	repoQuery   string
	context     string
	busy        bool
	// confirmed is set by the first enter on a row whose agent session is still open.
	confirmed bool
}

// Messages from background work.
type (
	loadedMsg struct {
		which     viewKind
		rows      []Row
		truncated bool // Jira or GitHub had more than the board loads
		err       error
	}
	detailTickMsg struct{ seq int }
	detailMsg     struct {
		key   string
		lines []string
		ok    bool
	}
	statusMsg struct {
		text  string
		color Color
		// progress lines come from a running dispatch; a load finishing doesn't clear other text.
		sticky bool
	}
	dispatchedMsg struct {
		text string
		err  error
	}
	refreshTickMsg struct{}
	agentsTickMsg  struct{}
	agentsMsg      struct {
		states map[string]string // nil when listing the sessions failed
		state  State
	}
	// formRowMsg carries a requested ticket the board hadn't loaded.
	formRowMsg struct {
		row Row
		err error
	}
	editedMsg struct {
		text string
		err  error
	}
	cleanupMsg struct {
		candidates []cleanupCandidate
		failed     []string // "KEY: why" for each worktree whose ticket or PR couldn't be checked
		err        error
	}
	cleanedMsg struct {
		removed, forgotten int // worktrees removed, dispatches without one forgotten
		errs               []string
	}
)

type model struct {
	jira  *Jira
	jql   string
	title string

	width, height int
	view          viewKind
	issues        []Row
	truncated     bool // the search matched more than maxIssues tickets
	prs           []Row
	prsLoaded     bool
	prsTruncated  bool   // the repos have more than maxPrs open PRs
	prFilter      string // "all" | "requested" | "mine"
	cursor, top   int
	query         string
	mine          bool
	assignee      *string // nil = everyone, "" = unassigned
	// Views with a load in flight. PRs load in the background from the start, so each view tracks its own.
	inFlight     map[viewKind]bool
	lastLoad     map[viewKind]time.Time
	status       string
	statusColor  Color
	statusSticky bool
	detailScroll int
	details      map[string][]string // nil value = loading
	detailOK     map[string]bool     // the cached detail is the real thing, not an error
	detailSeq    int
	state        State
	agents       map[string]string // Rex session ID or label → reported agent state
	repos        []string
	form         *form
	cleanup      *cleanupView
}

func (m *model) loading() bool { return m.inFlight[m.view] }

func (m *model) say(text string, c Color) { m.status, m.statusColor, m.statusSticky = text, c, false }

func (m *model) rows() []Row {
	if m.view == prs {
		return m.prs
	}
	return m.issues
}

func (m *model) reload(which viewKind) tea.Cmd {
	if m.inFlight[which] {
		return nil
	}
	m.inFlight[which] = true
	jira, jql := m.jira, m.jql
	return func() tea.Msg {
		if which == prs {
			list, truncated, err := openPrs()
			rows := make([]Row, len(list))
			for n, p := range list {
				rows[n] = prRow(p)
			}
			return loadedMsg{which, rows, truncated, err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
		defer cancel()
		list, truncated, err := jira.issues(ctx, jql)
		rows := make([]Row, len(list))
		for n, i := range list {
			rows[n] = issueRow(i)
		}
		return loadedMsg{which, rows, truncated, err}
	}
}

func (m *model) assignees() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.rows() {
		if !seen[r.Assignee] {
			seen[r.Assignee] = true
			out = append(out, r.Assignee)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if a == "" {
			return -1
		}
		if b == "" {
			return 1
		}
		return localeCompare(a, b)
	})
	return out
}

func (m *model) visible() []Row {
	return filterRows(m.rows(), m.query, m.prFilter, m.mine, m.assignee)
}

// filterRows keeps the rows that pass the view's filters and contain every word of the query.
func filterRows(rows []Row, query, prFilter string, mine bool, assignee *string) []Row {
	words := strings.Fields(strings.ToLower(query))
	var out []Row
	for _, r := range rows {
		if r.Pr != nil {
			if (prFilter == "requested" && !r.Pr.Requested) || (prFilter == "mine" && !r.Pr.Mine) {
				continue
			}
		} else if mine && !r.isMine() {
			continue
		}
		if assignee != nil && r.Assignee != *assignee {
			continue
		}
		hay := strings.ToLower(fmt.Sprintf("%s %s %s %s", r.Key, r.Status, cmp.Or(r.Assignee, "unassigned"), r.Summary))
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(hay, w) }) {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) current() (Row, bool) {
	items := m.visible()
	m.cursor = max(0, min(m.cursor, len(items)-1))
	if len(items) == 0 {
		return Row{}, false
	}
	return items[m.cursor], true
}

// scheduleDetail debounces loading the selected row's detail, so scrolling through the list doesn't fetch every row.
func (m *model) scheduleDetail() tea.Cmd {
	row, ok := m.current()
	if !ok || m.form != nil || m.cleanup != nil {
		return nil
	}
	if _, cached := m.details[row.Key]; cached {
		return nil
	}
	m.detailSeq++
	seq := m.detailSeq
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return detailTickMsg{seq} })
}

func (m *model) fetchDetail(row Row) tea.Cmd {
	m.details[row.Key] = nil
	jira := m.jira
	return func() tea.Msg {
		var md string
		var err error
		if row.Pr != nil {
			var v PrView
			if v, err = viewPr(*row.Pr); err == nil {
				md = prMarkdown(*row.Pr, v)
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
			defer cancel()
			md, err = jira.detail(ctx, row.Key)
		}
		if err != nil {
			return detailMsg{row.Key, []string{fg(cRed, fmt.Sprintf("Failed to load %s: %v", row.Key, err))}, false}
		}
		return detailMsg{row.Key, strings.Split(md, "\n"), true}
	}
}

func (m *model) repoChoices() []string {
	q := ""
	if m.form != nil {
		q = strings.ToLower(m.form.repoQuery)
	}
	var out []string
	for _, r := range m.repos {
		if q == "" || strings.Contains(strings.ToLower(tilde(r)), q) {
			out = append(out, r)
		}
	}
	return out
}

func project(key string) string { return strings.SplitN(key, "-", 2)[0] }

func (m *model) openForm(row Row) {
	m.form = &form{
		row:      row,
		worktree: true,
		repo:     max(0, slices.Index(m.repos, m.state.RepoByProject[project(row.Key)])),
	}
	m.followRepo()
}

// openTaskForm opens the form for a task, on the repo of the ticket under the cursor.
func (m *model) openTaskForm() {
	repo := 0
	if row, ok := m.current(); ok && row.Issue != nil {
		repo = max(0, slices.Index(m.repos, m.state.RepoByProject[project(row.Key)]))
	}
	m.form = &form{task: true, worktree: true, field: fieldContext, repo: repo}
	m.followRepo()
}

// formRepo is the main checkout the form would dispatch in: the PR's, or the picked repo.
func (m *model) formRepo() string {
	f := m.form
	if f.row.Pr != nil {
		return f.row.Pr.Checkout
	}
	choices := m.repoChoices()
	if len(choices) == 0 {
		return ""
	}
	return choices[max(0, min(f.repo, len(choices)-1))]
}

// followRepo puts the form on its repo's default agent, unless I picked one.
func (m *model) followRepo() {
	if f := m.form; !f.agentPicked {
		want := defaultAgent(m.state, m.formRepo())
		f.agent = max(0, slices.IndexFunc(agents, func(a Agent) bool { return a.Label == want }))
	}
}

// takeFormRequest opens the dispatch form the queue asked for, loading the ticket when the board's
// search doesn't hold it, and clears the request.
func (m *model) takeFormRequest() tea.Cmd {
	req := m.state.FormRequest
	if req == nil {
		return nil
	}
	m.state, _ = updateState(func(s *State) {
		if s.FormRequest != nil && s.FormRequest.Key == req.Key && s.FormRequest.At.Equal(req.At) {
			s.FormRequest = nil
		}
	})
	if time.Since(req.At) > formRequestTTL {
		return nil
	}
	if n := slices.IndexFunc(m.issues, func(r Row) bool { return r.Key == req.Key }); n >= 0 {
		m.openRequested(m.issues[n])
		return nil
	}
	m.say("Loading "+req.Key+"…", cSubtext)
	jira, key := m.jira, req.Key
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
		defer cancel()
		list, _, err := jira.issues(ctx, "key = "+key)
		if err == nil && len(list) == 0 {
			err = fmt.Errorf("%s isn't in Jira", key)
		}
		if err != nil {
			return formRowMsg{err: fmt.Errorf("%s: %w", key, err)}
		}
		return formRowMsg{row: issueRow(list[0])}
	}
}

// openRequested opens the form on a requested ticket, unless a dispatch is running from the form.
func (m *model) openRequested(row Row) {
	if m.form != nil && m.form.busy {
		m.say("A dispatch is running; "+row.Key+" waits until it ends, then press enter on its PR again", cYellow)
		return
	}
	m.cleanup = nil
	m.openForm(row)
	m.say("Dispatch "+row.Key+" from the queue", cSubtext)
}

var prog *tea.Program

func (m *model) submit() tea.Cmd {
	f := m.form
	choices := m.repoChoices()
	repo := ""
	if f.repo < len(choices) {
		repo = choices[f.repo]
	}
	if (repo == "" && f.row.Pr == nil) || f.busy {
		return nil
	}
	if f.task && strings.TrimSpace(f.context) == "" {
		f.field = fieldContext
		m.say("Describe the task first", cYellow)
		return nil
	}
	// A second agent would work in the first one's worktree, and the board would lose track of the first.
	if session := m.state.Sessions[f.row.Key]; session != "" && m.agents[session] != "" && !f.confirmed {
		f.confirmed = true
		m.say(fmt.Sprintf("%s's agent session is still open (%s); enter again starts a second agent", f.row.Key, m.agents[session]), cYellow)
		m.statusSticky = true
		return nil
	}
	f.busy = true
	agent := agents[f.agent].Label
	isTicket, agentRepo := f.row.Issue != nil, m.formRepo()
	m.state, _ = updateState(func(s *State) {
		if isTicket {
			s.RepoByProject[project(f.row.Key)] = repo
		}
		s.AgentByRepo[agentRepo] = agent
	})
	progress := func(line string) { prog.Send(statusMsg{line, cSubtext, true}) }
	jira, row, task, worktree, poteto, extra := m.jira, f.row, f.task, f.worktree, f.poteto, strings.TrimSpace(f.context)
	ticket := ""
	if m.detailOK[row.Key] && row.Pr == nil {
		ticket = strings.Join(m.details[row.Key], "\n")
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
		defer cancel()
		var text string
		var err error
		switch {
		case task:
			text, err = dispatchTask(TaskDispatch{Repo: repo, Agent: agent, Worktree: worktree, Poteto: poteto, Task: extra}, progress)
			if err != nil {
				err = fmt.Errorf("task: %w", err)
			}
			return dispatchedMsg{text, err}
		case row.Pr != nil:
			text, err = dispatchReview(ctx, jira, ReviewDispatch{*row.Pr, agent, extra}, progress)
		default:
			text, err = dispatch(ctx, jira, Dispatch{Issue: *row.Issue, Agent: agent, Worktree: worktree, Poteto: poteto, Repo: repo, Context: extra, Ticket: ticket}, progress)
		}
		if err != nil {
			err = fmt.Errorf("%s: %w", row.Key, err)
		}
		return dispatchedMsg{text, err}
	}
}

// pollAgents reads the agents' states and the state file: the queue and any other board write it too.
func pollAgents() tea.Msg {
	states, err := agentStates()
	if err != nil {
		states = nil
	}
	return agentsMsg{states, loadState()}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.reload(tickets), m.reload(prs), pollAgents, agentsTick(),
		tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} }))
}

// agentsTick schedules the next poll. Only agentsTickMsg reschedules it: extra polls (focus, a
// dispatch) must not each start another loop.
func agentsTick() tea.Cmd {
	return tea.Tick(agentsInterval, func(time.Time) tea.Msg { return agentsTickMsg{} })
}

// refreshStale reloads the views whose data is older than the refresh interval.
func (m *model) refreshStale() tea.Cmd {
	var cmds []tea.Cmd
	for _, v := range []viewKind{tickets, prs} {
		if time.Since(m.lastLoad[v]) >= refreshInterval {
			cmds = append(cmds, m.reload(v))
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case loadedMsg:
		delete(m.inFlight, msg.which)
		if msg.err != nil {
			name := "Tickets"
			if msg.which == prs {
				name = "PRs"
			}
			m.say(fmt.Sprintf("%s: %v", name, msg.err), cRed)
			break
		}
		m.lastLoad[msg.which] = time.Now()
		prev, had := m.current()
		if msg.which == prs {
			m.prs, m.prsLoaded, m.prsTruncated = msg.rows, true, msg.truncated
		} else {
			m.issues, m.truncated = msg.rows, msg.truncated
		}
		// A reload reorders the rows; without this the cursor ends up on another ticket.
		if had && msg.which == m.view {
			if n := slices.IndexFunc(m.visible(), func(r Row) bool { return r.Key == prev.Key }); n >= 0 {
				m.cursor = n
			} else {
				m.detailScroll = 0
			}
		}
		// PR keys hold a `#`, ticket keys don't: keep the other view's cached details.
		for key := range m.details {
			if strings.Contains(key, "#") == (msg.which == prs) {
				delete(m.details, key)
				delete(m.detailOK, key)
			}
		}
		// Clear the previous error or "loading" note, but not a dispatch's result or progress.
		if msg.which == m.view && !m.statusSticky {
			m.say("", cOverlay)
			switch {
			case msg.truncated && msg.which == prs:
				m.say(fmt.Sprintf("Showing the first %d open PRs; the rest aren't loaded", maxPrs), cYellow)
			case msg.truncated:
				m.say(fmt.Sprintf("Showing the first %d tickets; narrow the JQL to see the rest", maxIssues), cYellow)
			}
		}
	case detailTickMsg:
		if msg.seq != m.detailSeq || m.form != nil {
			return m, nil
		}
		if row, ok := m.current(); ok {
			if _, cached := m.details[row.Key]; !cached {
				return m, m.fetchDetail(row)
			}
		}
		return m, nil
	case detailMsg:
		m.details[msg.key] = msg.lines
		if msg.ok {
			m.detailOK[msg.key] = true
		} else {
			delete(m.detailOK, msg.key)
		}
	case statusMsg:
		m.say(msg.text, msg.color)
		m.statusSticky = msg.sticky
	case dispatchedMsg:
		if msg.err != nil {
			if m.form != nil {
				m.form.busy = false
			}
			m.say(msg.err.Error(), cRed)
		} else {
			m.state = loadState()
			m.form = nil
			m.say(msg.text, cGreen)
		}
		m.statusSticky = true
		return m, tea.Batch(pollAgents, m.scheduleDetail())
	case refreshTickMsg:
		next := tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
		return m, tea.Batch(next, m.refreshStale())
	case tea.FocusMsg:
		// Coming back to the board after a while: show what changed since.
		return m, tea.Batch(m.refreshStale(), pollAgents)
	case agentsTickMsg:
		return m, tea.Batch(pollAgents, agentsTick())
	case agentsMsg:
		m.state = msg.state
		if msg.states != nil {
			m.agents = msg.states
		}
		return m, m.takeFormRequest()
	case formRowMsg:
		if msg.err != nil {
			m.say(msg.err.Error(), cRed)
			break
		}
		m.openRequested(msg.row)
	case editedMsg:
		if msg.err != nil {
			m.say("Editor: "+msg.err.Error(), cRed)
		} else if m.form != nil {
			m.form.context = msg.text
		}
	case cleanupMsg:
		m.openCleanup(msg)
	case cleanedMsg:
		m.state = loadState()
		m.cleanup = nil
		done := fmt.Sprintf("Removed %d worktrees", msg.removed)
		if msg.forgotten > 0 {
			done += fmt.Sprintf(", forgot %d dispatches without one", msg.forgotten)
		}
		if len(msg.errs) > 0 {
			m.say(done+"; "+strings.Join(msg.errs, "; "), cRed)
		} else {
			m.say("✓ "+done, cGreen)
		}
		m.statusSticky = true
	case tea.PasteMsg:
		m.paste(msg.Content)
		return m, m.scheduleDetail()
	case tea.KeyPressMsg:
		var cmd tea.Cmd
		switch {
		case m.cleanup != nil:
			cmd = m.cleanupInput(msg)
		case m.form != nil:
			cmd = m.formInput(msg)
		default:
			cmd = m.boardInput(msg)
		}
		return m, tea.Batch(cmd, m.scheduleDetail())
	}
	return m, m.scheduleDetail()
}

const usage = `usage: foreman [PROJECT | JQL]
       foreman queue      every dispatched agent by state, and PRs that need you
       foreman dispatch   start an agent on a task, a ticket or a PR
       foreman orchestrator [--new]   Claude Code that hands work to agents (^g on the board)
       foreman agents | report | screen | reply | wait | cleanup
                          read and steer the agents from scripts
       foreman --logout   forget the Atlassian sign-in`

func run() error {
	argv := os.Args[1:]
	// The whole reference in one page: the orchestrator reads it unasked, and dispatch --help would ask.
	if len(argv) > 0 && (argv[0] == "--help" || argv[0] == "-h") {
		fmt.Println(usage + "\n\n" + dispatchUsage + "\n\n" + commandsUsage)
		return nil
	}
	if err := loadConfig(); err != nil {
		return err
	}
	if len(argv) > 0 && argv[0] == "queue" {
		return runQueue()
	}
	if len(argv) > 0 {
		rest := argv[1:]
		help := len(rest) > 0 && (rest[0] == "--help" || rest[0] == "-h")
		if help && argv[0] != "dispatch" && argv[0] != "queue" && argv[0] != "orchestrator" {
			fmt.Println(commandsUsage)
			return nil
		}
		switch argv[0] {
		case "dispatch":
			return runDispatch(rest)
		case "orchestrator":
			return runOrchestrator(rest)
		case "agents":
			return runAgents(rest, os.Stdout)
		case "report":
			return runReport(rest, os.Stdout)
		case "screen":
			return runScreen(rest, os.Stdout)
		case "reply":
			return runReply(rest, os.Stdin, os.Stdout)
		case "wait":
			return runWait(rest, os.Stdout)
		case "cleanup":
			return runCleanup(rest, os.Stdout)
		}
	}
	if len(argv) > 0 && argv[0] == "--logout" {
		signOut()
		return nil
	}

	jql, title := parseArgs(argv)
	// The board draws at once; Jira connects on the first call, signing in through the browser if needed.
	jira := &Jira{log: func(l string) { prog.Send(statusMsg{strings.ReplaceAll(l, "\n", " "), cYellow, true}) }}
	defer jira.close()

	m := &model{
		jira:        jira,
		jql:         jql,
		title:       title,
		prFilter:    "all",
		inFlight:    map[viewKind]bool{},
		lastLoad:    map[viewKind]time.Time{},
		statusColor: cOverlay,
		details:     map[string][]string{},
		detailOK:    map[string]bool{},
		state:       loadState(),
		agents:      map[string]string{},
		repos:       listRepos(),
	}
	prog = tea.NewProgram(m)
	go func() {
		started, err := startWatchers()
		switch {
		case err != nil:
			prog.Send(statusMsg{err.Error(), cRed, true})
		case len(started) > 0:
			prog.Send(statusMsg{"Started " + strings.Join(started, ", "), cSubtext, false})
		}
	}()
	_, err := prog.Run()
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
