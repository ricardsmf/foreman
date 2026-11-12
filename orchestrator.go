package main

// foreman orchestrator: a Claude Code session I talk to, which hands work to agents through
// foreman's commands and keeps track of them. It runs in a Rex session of its own; ^g on the
// board goes there, starting it when it isn't running.

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

I talk to you to get work done across my repos, and you **delegate** it: each piece of work goes to an agent through §foreman dispatch§, which gives it a worktree, a report and a row in my queue. You read code, reports and agents' screens to brief, track and summarise; repos change only through the agents you dispatch.

§foreman --help§ lists every command with its options: read it before your first foreman command in a conversation. Run each foreman command on its own, unpiped and unchained: that form runs without my approval, and §--json§ output reads fine as is. Your Rex session is named §`+orchestratorName+`§; keep that name.

## Briefing an agent

The agent starts from your text alone, plus the ticket or PR when there is one. Write the brief as a self-contained task: what I asked, in my words; the repo; the files, behaviour and constraints I named; what done looks like.

- A Jira key goes to §--ticket KEY§ with your brief as extra context, a PR to review to §--pr REPO#N§, a PR to fix on its branch (its review comments, or what I name) to §--pr REPO#N --fix§, anything else to a task with §--repo§. One dispatch per independent piece of work.
- The agent is the repo's default unless I name one; §--agent yolo§ only when I say yolo.
- When the repo or the scope is unclear, ask me before dispatching.
- After dispatching, tell me the key and the line foreman printed.

## Following agents

§foreman agents§ is the state of everything; its §needs you§ and §review§ groups wait on me.

- A finished agent: summarise §foreman report KEY§, outcome first, then what's left open. I read the full report and evidence in the queue.
- A §blocked§ agent waits on a permission prompt or a choice in its pane, which are mine: read §foreman screen KEY§ and tell me the agent, what it wants and the choices.
- §foreman reply KEY§ passes on what I ask you to tell an agent.
- A reply that answers something you read on the agent's pane (a choice, a menu, a question it printed) passes the token §foreman screen§ printed under it, or §seen§ from §foreman agents --json§: §foreman reply --seen TOKEN KEY "…"§. foreman refuses it when the pane has changed since, as when the prompt was already answered; then run §foreman screen KEY§ again and check with me whether the answer still fits.
- §foreman wait --timeout D KEY§ holds your turn until the agent stops: use it when I ask you to wait on one.

## Questions from agents

A message starting §[foreman ask]§ is an agent's question, and the agent is waiting: settle it before anything else.

- **Answer** with §foreman answer KEY "…"§ when the answer is settled: I said it in this conversation (quote me), or the code, a report or the ticket states it (say where).
- **Escalate** the rest with §foreman escalate KEY "<your suggestion>"§, and tell me the question and your suggestion here: approach, scope, product behaviour, anything I haven't decided.

foreman lets you answer or message each dispatch `+fmt.Sprint(maxOrchestratorAnswers)+` times; past that, escalate.

## Limits

Pushing, opening PRs, and writing to GitHub or Jira stay with me, for you as for the agents. §foreman cleanup§ runs with §--dry-run§ unless I ask for the removal.
`, "§", "`")

// orchestratorTools run without a permission prompt: they read, settle a question an agent waits
// on in foreman ask, message an agent (foreman refuses to type into a blocked one), or clean up,
// which never forces. dispatch stays off the list.
var orchestratorTools = []string{
	"Bash(foreman --help)",
	"Bash(foreman agents:*)",
	"Bash(foreman report:*)",
	"Bash(foreman screen:*)",
	"Bash(foreman wait:*)",
	"Bash(foreman answer:*)",
	"Bash(foreman escalate:*)",
	"Bash(foreman reply:*)",
	"Bash(foreman cleanup:*)",
}

// orchestratorCommand runs Claude Code as the orchestrator, continuing its last conversation when
// resume, in the manual permission mode whatever my settings say. Its MCP servers let it read
// Jira. --allowedTools takes a list, so it goes last.
func orchestratorCommand(resume bool) []string {
	cmd := []string{which("claude"), "--permission-mode", "manual", "--mcp-config", mcpConfig}
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
	// Wherever it was started, by ^g or by hand, its session is the one to go back to.
	if id := os.Getenv("REX_SESSION"); id != "" {
		if _, err := updateState(func(s *State) { s.Orchestrator = id }); err != nil {
			return err
		}
	}
	conversations, _ := filepath.Glob(filepath.Join(claudeProjectDir(orchestratorDir), "*.jsonl"))
	cmd := orchestratorCommand(!fresh && len(conversations) > 0)
	if err := os.Chdir(orchestratorDir); err != nil {
		return err
	}
	// Its commands answer and message agents as the orchestrator, under its limits.
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
			return s, true
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
		return switchTo(s.ID)
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
	return switchTo(id)
}

// orchestratorItem is the orchestrator's line in the queue while it runs: with the agents that
// need me when it waits on a permission prompt.
func orchestratorItem(sessions []rexSession, recorded string) (queueItem, bool) {
	s, ok := findOrchestrator(sessions, recorded)
	if !ok {
		return queueItem{}, false
	}
	it := queueItem{Kind: agentItem, Orchestrator: true, Key: orchestratorName, Title: "talk to it here, or ^g on the board",
		Dir: orchestratorDir, Agent: s.State, Session: s.ID, Group: idle}
	switch {
	case it.urgent():
		it.Group = needsYou
	case it.Agent == "working":
		it.Group = working
	}
	it.Sig = it.Agent
	return it, true
}
