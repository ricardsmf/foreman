package main

// foreman orchestrator: a Claude Code session I talk to, which hands work to agents through
// foreman's commands and keeps track of them. It runs in a Rex session of its own; ^g on the
// board goes there, starting it when it isn't running.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const orchestratorName = "foreman-orchestrator"

// orchestratorDir is where it runs: outside every repo, so the work it does lands in agents'
// worktrees. Its CLAUDE.md is rewritten on each start, to match this build's commands.
var orchestratorDir = filepath.Join(home, ".local", "state", "foreman", "orchestrator")

const orchestratorGuide = `# Foreman orchestrator

I talk to you to get work done across my repos, and you **delegate** it: each piece of work goes to an agent through ` + "`foreman dispatch`" + `, which gives it a worktree, a report and a row in my queue. You read code, reports and agents' screens to brief, track and summarise; repos change only through the agents you dispatch.

` + "`foreman --help`" + ` lists every command with its options: read it before your first foreman command in a conversation. Your Rex session is named ` + "`" + orchestratorName + "`" + `; keep that name.

## Briefing an agent

The agent starts from your text alone, plus the ticket or PR when there is one. Write the brief as a self-contained task: what I asked, in my words; the repo; the files, behaviour and constraints I named; what done looks like.

- A Jira key goes to ` + "`--ticket KEY`" + ` with your brief as extra context, a PR to ` + "`--pr REPO#N`" + `, anything else to a task with ` + "`--repo`" + `. One dispatch per independent piece of work.
- The agent is the repo's default unless I name one; ` + "`--agent yolo`" + ` only when I say yolo.
- When the repo or the scope is unclear, ask me before dispatching.
- After dispatching, tell me the key and the line foreman printed.

## Following agents

` + "`foreman agents`" + ` is the state of everything; its ` + "`needs you`" + ` and ` + "`review`" + ` groups wait on me.

- A finished agent: summarise ` + "`foreman report KEY`" + `, outcome first, then what's left open. I read the full report and evidence in the queue.
- An agent that asks (` + "`blocked`" + `): read ` + "`foreman screen KEY`" + `. A permission prompt, a command or edit awaiting approval, is mine to answer: tell me the agent, what it wants to run and the choices. A question about the work you answer with ` + "`foreman reply KEY`" + ` when I've already told you the answer, quoting me; otherwise bring it to me with your suggestion.
- ` + "`foreman wait --timeout D KEY`" + ` holds your turn until the agent stops: use it when I ask you to wait on one.

## Limits

Pushing, opening PRs, and writing to GitHub or Jira stay with me, for you as for the agents. ` + "`foreman cleanup`" + ` runs with ` + "`--dry-run`" + ` unless I ask for the removal.
`

// orchestratorTools run without a permission prompt: they only read.
var orchestratorTools = []string{
	"Bash(foreman --help)",
	"Bash(foreman agents:*)",
	"Bash(foreman report:*)",
	"Bash(foreman screen:*)",
	"Bash(foreman wait:*)",
}

// orchestratorAsks always ask, so I see every command it sends to an agent: a reply could answer
// another agent's permission prompt. Manual mode alone isn't enough: Claude Code runs some
// commands it judges safe unasked, foreman dispatch among them.
var orchestratorAsks = []string{
	"Bash(foreman dispatch:*)",
	"Bash(foreman reply:*)",
	"Bash(foreman cleanup:*)",
}

// orchestratorCommand runs Claude Code as the orchestrator, continuing its last conversation when
// resume, in the manual permission mode whatever my settings say. Its MCP servers let it read
// Jira. --allowedTools takes a list, so it goes last.
func orchestratorCommand(resume bool) []string {
	asks, _ := json.Marshal(map[string]any{"permissions": map[string]any{"ask": orchestratorAsks}})
	cmd := []string{which("claude"), "--permission-mode", "manual", "--settings", string(asks), "--mcp-config", mcpConfig}
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
	return syscall.Exec(cmd[0], cmd, os.Environ())
}

// findOrchestrator is the orchestrator's open session: the recorded one, else one labelled
// exactly its name. Not by prefix, as agents are found: foreman-orchestrator-… may be anything.
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
