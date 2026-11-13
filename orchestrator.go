package main

// foreman orchestrator: a Claude Code session I talk to, which briefs agents through foreman's
// commands and brings their work together. It runs in a Rex session of its own; ^g on the board
// goes there, starting it when it isn't running.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const orchestratorName = "foreman-orchestrator"

// orchestratorDir is where it runs: outside every repo, so the work it does lands in agents'
// worktrees. Its CLAUDE.md is rewritten on each start, to match this build's commands.
var orchestratorDir = filepath.Join(home, ".local", "state", "foreman", "orchestrator")

// orchestratorGuide is the orchestrator's CLAUDE.md; § stands for a backtick, which a Go raw string can't hold.
var orchestratorGuide = strings.ReplaceAll(`# Foreman orchestrator

I talk to you to get work done across my repos. A change to a repo's code goes to an agent through §foreman dispatch§, which gives it a worktree, a report and a row in my queue. Here you brief the agents, bring their finished work together, and answer what I ask about code, reports and tickets.

My queue, in the foreman pane, shows me every agent, its report, and what it waits on, and I answer agents there. Tell me what it can't show: what several reports add up to, where they overlap, what to do next.

§foreman --help§ lists every command with its options: read it before your first foreman command in a conversation. Run each foreman command on its own, unpiped and unchained: that form runs without my approval. Your Rex session is named §`+orchestratorName+`§; keep that name.

## Briefing an agent

The agent starts from your text alone, plus the ticket or PR when there is one. Write the brief as a self-contained task: what I asked, in my words; the repo; the files, behaviour and constraints I named; what done looks like.

- A Jira key goes to §--ticket KEY§ with your brief as extra context, a PR to review to §--pr REPO#N§, a PR to fix on its branch (its review comments, or what I name) to §--pr REPO#N --fix§, anything else to a task with §--repo§. One dispatch per independent piece of work.
- The agent is the repo's default unless I name one; §--agent yolo§ only when I say yolo.
- When the repo or the scope is unclear, ask me before dispatching.
- After dispatching, tell me the key and the line foreman printed.

## Following agents

§foreman wait KEY§ holds your turn until the agent stops and prints why: use it when I ask you to wait, one call per agent.

- **Done**: summarise §foreman report KEY§, outcome first, then what's left open. For several, add where they touch the same files or depend on each other's choices.
- **Blocked or asked**: the agent waits on me, and I answer on its row in the queue. Tell me in a line which agent and what it wants, with what the command would change when its text doesn't say, then wait on it again.
- What I tell you to pass on goes with §foreman answer KEY§ to an agent that asked, with §foreman reply KEY§ to any other. Write it to stand alone: my words, with what "yes" or "the second option" refers to spelled out.
- A reply to something you read on an agent's pane passes the token §foreman screen§ or §foreman wait§ printed under it: §foreman reply --seen TOKEN KEY "…"§. foreman refuses it when the pane has changed since; then read the pane again and check with me whether the answer still fits.

## Bringing work together

Agents commit on their branch and stop there. Merging, pushing, opening a PR and writing to Jira happen here, each when I ask for it.

- Merge finished branches one at a time, running the repo's build and tests after each.
- §foreman cleanup§ runs with §--dry-run§ unless I ask for the removal.
`, "§", "`")

// orchestratorTools run without a permission prompt: they read, pass my answer to an agent that
// waits in foreman ask, message an agent (foreman refuses to type into a blocked one), or clean
// up, which never forces. dispatch stays off the list.
var orchestratorTools = []string{
	"Bash(foreman --help)",
	"Bash(foreman agents:*)",
	"Bash(foreman report:*)",
	"Bash(foreman screen:*)",
	"Bash(foreman wait:*)",
	"Bash(foreman answer:*)",
	"Bash(foreman reply:*)",
	"Bash(foreman cleanup:*)",
}

// manualMode starts Claude Code in its manual permission mode (config value "default") and keeps
// it there: a tool its allowlists don't name prompts. The flag alone only sets the starting mode:
// manual mode's Bash prompt puts the cursor on "Yes, and switch to auto mode", so one enter hands
// the session to the auto-mode classifier. disableAutoMode, passed with --settings, which outranks
// every settings file but managed ones, takes auto out of that prompt and the shift+tab cycle.
var manualMode = []string{"--permission-mode", "manual", "--settings", `{"disableAutoMode":"disable"}`}

// orchestratorCommand runs Claude Code as the orchestrator, continuing its last conversation when
// resume, in manualMode. Its MCP servers let it read Jira. --allowedTools takes a list, so it
// goes last.
func orchestratorCommand(resume bool) []string {
	cmd := append([]string{which("claude")}, manualMode...)
	cmd = append(cmd, "--mcp-config", mcpConfig)
	if resume {
		cmd = append(cmd, "--continue")
	}
	return append(append(cmd, "--allowedTools"), orchestratorTools...)
}

// runOrchestrator writes the orchestrator's instructions and replaces this process with Claude
// Code, continuing the last conversation unless --new.
func runOrchestrator(argv []string) error {
	const usage = "usage: foreman orchestrator [--new]   start Claude Code as the orchestrator, continuing its last conversation"
	if len(argv) == 1 && (argv[0] == "--help" || argv[0] == "-h") {
		fmt.Println(usage)
		return nil
	}
	fresh := len(argv) == 1 && argv[0] == "--new"
	if len(argv) > 1 || (len(argv) == 1 && !fresh) {
		return errors.New(usage)
	}
	if err := os.MkdirAll(orchestratorDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(orchestratorDir, "CLAUDE.md"), []byte(orchestratorGuide), 0o644); err != nil {
		return err
	}
	// Wherever it was started, by ^g or by hand, its session is the one to go back to, and this
	// pane the one it answers in.
	if id := os.Getenv("REX_SESSION"); id != "" {
		block := os.Getenv("REX_BLOCK")
		if _, err := updateState(func(s *State) {
			s.Orchestrator = id
			if block != "" {
				s.Panes[id] = block
			}
		}); err != nil {
			return err
		}
	}
	conversations, _ := filepath.Glob(filepath.Join(claudeProjectDir(orchestratorDir), "*.jsonl"))
	cmd := orchestratorCommand(!fresh && len(conversations) > 0)
	if err := os.Chdir(orchestratorDir); err != nil {
		return err
	}
	// Its commands message agents as the orchestrator: foreman refuses to type into a blocked one.
	return syscall.Exec(cmd[0], cmd, append(os.Environ(), roleEnv+"="+byOrchestrator))
}

// findOrchestrator is the orchestrator's open session: the recorded one, else one labelled
// exactly its name, else one running Claude Code in its directory. Not by prefix, as agents are
// found: foreman-orchestrator-… may be anything.
func findOrchestrator(sessions []rexSession, recorded string) (rexSession, bool) {
	for _, s := range sessions {
		if recorded != "" && s.ID == recorded {
			return s, true
		}
	}
	for _, s := range sessions {
		if s.Label == orchestratorName {
			return s, true
		}
	}
	for _, s := range sessions {
		if s.runsClaudeIn(orchestratorDir) {
			return s.onClaudeIn(orchestratorDir), true
		}
	}
	return rexSession{}, false
}

// goToOrchestrator shows the orchestrator's session, starting it when it isn't running.
func goToOrchestrator() error {
	sessions, err := agentSessions()
	if err != nil {
		return err
	}
	if s, ok := findOrchestrator(sessions, loadState().Orchestrator); ok {
		return rex.show(s.ID)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(orchestratorDir, 0o755); err != nil {
		return err
	}
	id, _, err := newSession(orchestratorName, orchestratorDir, agentClaude, []string{exe, "orchestrator"})
	if err != nil {
		return err
	}
	return rex.show(id)
}

// orchestratorItem is the orchestrator's line in the queue while it runs: with the agents that
// need me when it waits on a permission prompt.
func orchestratorItem(sessions []rexSession, recorded string) (queueItem, bool) {
	s, ok := findOrchestrator(sessions, recorded)
	if !ok {
		return queueItem{}, false
	}
	it := queueItem{Kind: agentItem, Orchestrator: true, Key: orchestratorName, Title: "talk to it here, or ^g on the board",
		Dir: orchestratorDir, Group: idle}
	it.on(s)
	switch {
	case it.urgent():
		it.Group = needsYou
	case it.Agent == "working":
		it.Group = working
	}
	it.Sig = it.Agent
	return it, true
}
