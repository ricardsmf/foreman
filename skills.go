package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A repo's own Claude Code skills live in .claude/skills/<name>/SKILL.md. pi doesn't load them, so
// a repo that has any starts on Claude, and Claude's prompt names the ones it may invoke.

const (
	agentPi     = "pi"
	agentClaude = "Claude"
)

var manualSkill = regexp.MustCompile(`(?m)^disable-model-invocation:\s*true\s*$`)

// repoSkills lists the skills in dir's checkout that an agent can invoke by itself, sorted. Those
// marked disable-model-invocation are left out: only I can run them.
func repoSkills(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, ".claude", "skills", "*", "SKILL.md"))
	var names []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// The frontmatter sits between the first two --- lines.
		head, _, _ := strings.Cut(strings.TrimPrefix(string(b), "---"), "\n---")
		if !manualSkill.MatchString(head) {
			names = append(names, filepath.Base(filepath.Dir(f)))
		}
	}
	slices.Sort(names)
	return names
}

// hasSkills is whether the repo ships any Claude Code skill, invocable by the agent or not.
func hasSkills(repo string) bool {
	files, _ := filepath.Glob(filepath.Join(repo, ".claude", "skills", "*", "SKILL.md"))
	return len(files) > 0
}

// defaultAgent is the agent the form starts on for a repo: the one last dispatched there, else
// Claude where the repo ships Claude Code skills, else pi.
func defaultAgent(s State, repo string) string {
	if last := s.AgentByRepo[repo]; slices.ContainsFunc(agents, func(a Agent) bool { return a.Label == last }) {
		return last
	}
	if hasSkills(repo) {
		return agentClaude
	}
	return agentPi
}

// skillsSection tells Claude which of the repo's skills cover its work. pi gets nothing: it can't
// invoke them.
func skillsSection(agent, cwd string) []string {
	skills := repoSkills(cwd)
	if agent == agentPi || len(skills) == 0 {
		return nil
	}
	return []string{
		"",
		"## Repo skills",
		"",
		"This repo ships its own skills: `" + strings.Join(skills, "`, `") + "`. Where one covers a step of the workflow below, invoke it with the Skill tool instead of improvising that step.",
		"The workflow's limits win over a skill's: leave out any part of a skill that pushes, opens a PR, or posts to GitHub or Jira.",
	}
}
