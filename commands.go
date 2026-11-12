package main

// The queue's reads and replies as commands, for scripts and for an agent that keeps track of the
// others: foreman agents, report, screen, reply, wait and cleanup.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const commandsUsage = `usage: foreman agents [--json] [--prs]   every dispatched agent and watcher, by queue group
       foreman report KEY                the agent's report and its evidence files
       foreman screen [--lines N] KEY     the end of the agent's pane (0: all of it)
       foreman reply [--now] KEY TEXT|-   type TEXT into the agent's pane and press enter (options before KEY)
       foreman wait [--timeout D] KEY     block until the agent stops working
       foreman cleanup [--dry-run] [KEY…] remove finished worktrees, as ^x on the board

KEY is a dispatch's key (FLD-1700, reports-app#1185, task:foreman-3) or a watcher's name, in any case.`

// An agentInfo is a queue item as the agents command reports it.
type agentInfo struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`  // ticket, review, task, watcher, pr
	Group string `json:"group"` // needs you, review, working, idle, closed
	// State is what the agent reports to Rex (working, blocked, error, done, idle, open), or
	// closed when its session is.
	State       string     `json:"state"`
	Title       string     `json:"title"`
	Agent       string     `json:"agent,omitempty"` // pi, Claude, Claude (yolo)
	Session     string     `json:"session,omitempty"`
	Dir         string     `json:"dir,omitempty"`
	Report      string     `json:"report,omitempty"`
	ReportAt    *time.Time `json:"reportAt,omitempty"`
	Screenshots int        `json:"screenshots,omitempty"`
	Videos      int        `json:"videos,omitempty"`
	URL         string     `json:"url,omitempty"`    // a PR's
	Status      string     `json:"status,omitempty"` // a PR's: approved, changes, conflict…
}

func kindName(it queueItem) string {
	switch {
	case it.Kind == prItem:
		return "pr"
	case it.Watcher:
		return "watcher"
	case it.Orchestrator:
		return "orchestrator"
	case it.review():
		return "review"
	case isTask(it.Key):
		return "task"
	}
	return "ticket"
}

func agentInfoOf(it queueItem, s State) agentInfo {
	a := agentInfo{
		Key: it.Key, Kind: kindName(it), Group: groupNames[it.Group], State: cmp.Or(it.Agent, "closed"),
		Title: it.Title, Session: it.Session, Dir: it.Dir, Report: it.Report,
		Screenshots: it.Shots, Videos: it.Videos,
	}
	if it.Kind == prItem {
		a.State, a.Session, a.URL, a.Status = "", "", it.Pr.URL, it.Pr.Status
		return a
	}
	switch {
	case it.Orchestrator:
		a.Agent = agentClaude
	case !it.Watcher:
		a.Agent = s.Agents[it.Key]
	}
	if it.Report != "" {
		at := it.When
		a.ReportAt = &at
	}
	return a
}

// liveItems is the queue's list without PRs, from the state file and the open sessions.
func liveItems() (State, []queueItem, error) {
	sessions, err := agentSessions()
	if err != nil {
		return State{}, nil, fmt.Errorf("listing Rex sessions: %w", err)
	}
	s := loadState()
	return s, queueItems(s, sessions, nil), nil
}

// findItem is the agent or watcher with the key, in any case.
func findItem(items []queueItem, key string) (queueItem, error) {
	for _, it := range items {
		if it.Kind == agentItem && strings.EqualFold(it.Key, key) {
			return it, nil
		}
	}
	return queueItem{}, fmt.Errorf("no dispatched agent or watcher %q; foreman agents lists them", key)
}

func lookup(key string) (State, queueItem, error) {
	s, items, err := liveItems()
	if err != nil {
		return s, queueItem{}, err
	}
	it, err := findItem(items, key)
	return s, it, err
}

func runAgents(argv []string, out io.Writer) error {
	fs := newFlags("agents")
	asJSON := fs.Bool("json", false, "")
	withPrs := fs.Bool("prs", false, "")
	if _, err := parseFlags(fs, argv, 0); err != nil {
		return err
	}
	sessions, err := agentSessions()
	if err != nil {
		return fmt.Errorf("listing Rex sessions: %w", err)
	}
	var prs []Pr
	if *withPrs {
		if prs, _, err = openPrs(); err != nil {
			return fmt.Errorf("PRs: %w", err)
		}
	}
	s := loadState()
	infos := []agentInfo{}
	for _, it := range queueItems(s, sessions, prs) {
		infos = append(infos, agentInfoOf(it, s))
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(infos)
	}
	return writeAgents(out, infos)
}

// writeAgents prints one agent a line: group, key, state, title.
func writeAgents(out io.Writer, infos []agentInfo) error {
	keyW := 4
	for _, a := range infos {
		keyW = max(keyW, len(a.Key))
	}
	for _, a := range infos {
		state := a.State
		if a.Kind == "pr" {
			state = a.Status
		}
		note := ""
		if a.Report != "" {
			note = "  report"
		}
		if _, err := fmt.Fprintf(out, "%-9s  %-*s  %-8s  %s%s\n", a.Group, keyW, a.Key, state, a.Title, note); err != nil {
			return err
		}
	}
	return nil
}

func runReport(argv []string, out io.Writer) error {
	fs := newFlags("report")
	args, err := parseFlags(fs, argv, 1)
	if err != nil {
		return err
	}
	_, it, err := lookup(args[0])
	if err != nil {
		return err
	}
	if it.Report == "" {
		return fmt.Errorf("%s has written no report yet (%s)", it.Key, cmp.Or(it.Agent, "closed"))
	}
	b, err := os.ReadFile(it.Report)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, strings.TrimRight(string(b), "\n"))
	media, captures := evidence(filepath.Dir(it.Report))
	if files := append(media, captures...); len(files) > 0 {
		fmt.Fprint(out, "\n## Evidence\n\n")
		for _, f := range files {
			fmt.Fprintln(out, "- "+f)
		}
	}
	return nil
}

func runScreen(argv []string, out io.Writer) error {
	fs := newFlags("screen")
	lines := fs.Int("lines", 40, "")
	args, err := parseFlags(fs, argv, 1)
	if err != nil {
		return err
	}
	_, it, err := lookup(args[0])
	if err != nil {
		return err
	}
	if it.Agent == "" {
		return fmt.Errorf("%s's session is closed", it.Key)
	}
	block, err := agentBlock(it.Session)
	if err != nil {
		return err
	}
	screen, err := sh("", "rex", "capture", "-s", it.Session, "-b", block, "--trim")
	if err != nil {
		return err
	}
	all := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if *lines > 0 {
		all = all[max(0, len(all)-*lines):]
	}
	fmt.Fprintln(out, strings.Join(all, "\n"))
	return nil
}

func runReply(argv []string, stdin io.Reader, out io.Writer) error {
	fs := newFlags("reply")
	now := fs.Bool("now", false, "")
	if err := fs.Parse(argv); err != nil {
		return usageError(err)
	}
	if fs.NArg() < 2 {
		return usageError(errors.New("reply takes KEY and TEXT"))
	}
	text := strings.Join(fs.Args()[1:], " ")
	if text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if text = strings.TrimSpace(text); text == "" {
		return errors.New("nothing to send")
	}
	_, it, err := lookup(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := replyAllowed(it, *now); err != nil {
		return err
	}
	if err := sendToAgent(it.Session, text); err != nil {
		return fmt.Errorf("reply to %s: %w", it.Key, err)
	}
	fmt.Fprintln(out, "✓ Sent to "+it.Key)
	return nil
}

// replyAllowed refuses a reply to a closed session, and to a working agent unless now: pi and
// Claude Code each handle text typed mid-turn their own way.
func replyAllowed(it queueItem, now bool) error {
	switch {
	case it.Agent == "":
		return fmt.Errorf("%s's session is closed; enter on it in the queue resumes it", it.Key)
	case it.Agent == "working" && !now:
		return fmt.Errorf("%s is working; foreman wait %s, then reply, or pass --now", it.Key, it.Key)
	}
	return nil
}

func runWait(argv []string, out io.Writer) error {
	fs := newFlags("wait")
	timeout := fs.Duration("timeout", 30*time.Minute, "")
	args, err := parseFlags(fs, argv, 1)
	if err != nil {
		return err
	}
	key := args[0]
	look := func() (queueItem, error) {
		_, it, err := lookup(key)
		return it, err
	}
	it, err := waitIdle(look, *timeout, waitSettle, waitPoll, time.Sleep)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s\n", it.Key, cmp.Or(it.Agent, "closed"))
	return nil
}

const (
	waitPoll = 2 * time.Second
	// waitSettle is how long wait gives an agent that isn't working to start: a reply sent just
	// before takes a moment to show as working.
	waitSettle = 10 * time.Second
)

// waitIdle polls look until the agent stops working: it asks, fails, finishes or its session
// closes. An agent not working at the start gets settle to begin.
func waitIdle(look func() (queueItem, error), timeout, settle, poll time.Duration, sleep func(time.Duration)) (queueItem, error) {
	started := false
	for waited := time.Duration(0); ; waited += poll {
		it, err := look()
		if err != nil {
			return it, err
		}
		if it.Agent == "working" {
			started = true
		} else if started || waited >= settle {
			return it, nil
		}
		if waited >= timeout {
			return it, fmt.Errorf("%s is still working after %s", it.Key, timeout)
		}
		sleep(poll)
	}
}

func runCleanup(argv []string, out io.Writer) error {
	fs := newFlags("cleanup")
	dryRun := fs.Bool("dry-run", false, "")
	keys, err := parseFlags(fs, argv, -1)
	if err != nil {
		return err
	}
	states, err := agentStates()
	if err != nil {
		return fmt.Errorf("listing Rex sessions: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), jiraTimeout)
	defer cancel()
	jira := &Jira{log: func(l string) { fmt.Fprintln(os.Stderr, l) }}
	defer jira.close()
	found := cleanupCandidates(ctx, jira, loadState(), states)
	if found.err != nil {
		return found.err
	}
	for _, f := range found.failed {
		fmt.Fprintln(os.Stderr, "couldn't check "+f)
	}
	picked, err := pickCandidates(found.candidates, keys)
	if err != nil {
		return err
	}
	for _, c := range found.candidates {
		mark := "  "
		if containsKey(picked, c.Key) {
			mark = "✓ "
		}
		fmt.Fprintf(out, "%s%s  %s  %s\n", mark, c.Key, tilde(c.Dir), strings.Join(candidateNotes(c), " · "))
	}
	if len(found.candidates) == 0 {
		fmt.Fprintln(out, "Nothing to clean up: every dispatched ticket, PR and task is still open")
	}
	if *dryRun || len(picked) == 0 {
		return nil
	}
	done := removeCandidates(picked)
	fmt.Fprintf(out, "Removed %d worktrees, forgot %d dispatches without one\n", done.removed, done.forgotten)
	if len(done.errs) > 0 {
		return errors.New(strings.Join(done.errs, "; "))
	}
	return nil
}

// pickCandidates is the candidates named by keys, or with none named, those ^x ticks unasked.
func pickCandidates(all []cleanupCandidate, keys []string) ([]cleanupCandidate, error) {
	var picked []cleanupCandidate
	if len(keys) == 0 {
		for _, c := range all {
			if defaultPicked(c) {
				picked = append(picked, c)
			}
		}
		return picked, nil
	}
	for _, k := range keys {
		n := indexKey(all, k)
		if n < 0 {
			return nil, fmt.Errorf("%s isn't ready to clean up: its ticket is open, its PR open or its session running", k)
		}
		picked = append(picked, all[n])
	}
	return picked, nil
}

func indexKey(cs []cleanupCandidate, key string) int {
	for n, c := range cs {
		if strings.EqualFold(c.Key, key) {
			return n
		}
	}
	return -1
}

func containsKey(cs []cleanupCandidate, key string) bool { return indexKey(cs, key) >= 0 }

func candidateNotes(c cleanupCandidate) []string {
	notes := []string{c.Reason}
	if c.Main {
		notes = append(notes, "checkout stays")
	}
	if c.Dirty {
		notes = append(notes, "uncommitted changes")
	}
	if c.Agent != "" {
		notes = append(notes, "agent "+c.Agent)
	}
	return notes
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses argv, options before or after the keys, and returns the keys: exactly args
// of them, or any number when args is -1.
func parseFlags(fs *flag.FlagSet, argv []string, args int) ([]string, error) {
	var keys []string
	for {
		if err := fs.Parse(argv); err != nil {
			return nil, usageError(err)
		}
		if fs.NArg() == 0 {
			break
		}
		keys, argv = append(keys, fs.Arg(0)), fs.Args()[1:]
	}
	if args >= 0 && len(keys) != args {
		return nil, usageError(fmt.Errorf("%s takes %d argument(s), got %d", fs.Name(), args, len(keys)))
	}
	return keys, nil
}

func usageError(err error) error { return fmt.Errorf("%w\n\n%s", err, commandsUsage) }
