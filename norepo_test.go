package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	lua "github.com/yuin/gopher-lua"
)

// A task with no repo runs in a scratch directory of its own, keyed task:<n> apart from the repos'
// numbers, and its agent answers in the report with nothing to commit.
func TestTaskWithoutARepo(t *testing.T) {
	f := useFakeRex(t)
	tempState(t)
	if err := runDispatch([]string{"--agent", "pi", "What does the queue's g key do?"}); err != nil {
		t.Fatal(err)
	}
	d, ok := loadState().Dispatched["task:1"]
	want := filepath.Join(filepath.Dir(stateFile), "tasks", "task-1")
	if !ok || d.Dir != want || !exists(d.Dir) || mainCheckout(d.Dir) != "" || d.Agent != agentPi {
		t.Fatalf("dispatched %+v (%t), want it in %s", d, ok, want)
	}
	if _, ok := f.cmds["task-1-what-does-the"]; !ok {
		t.Errorf("sessions started: %v", f.cmds)
	}
	b, err := os.ReadFile(promptFile("task:1"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(b)
	for _, want := range []string{
		`# task:1: "What does the queue's g key do?"`,
		"no repo",
		"Working directory: " + want,
		"## Task\n\nWhat does the queue's g key do?",
		"foreman ask",
		evidenceDir("task:1") + "/report.md",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, not := range []string{"Branch:", "**Commit**", "foreman oracle"} {
		if strings.Contains(prompt, not) {
			t.Errorf("the prompt has %q:\n%s", not, prompt)
		}
	}

	if err := runDispatch([]string{"--agent", "pi", "another"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadState().Dispatched["task:2"]; !ok {
		t.Error("the second task with no repo isn't task:2")
	}
	if key, _, _ := newTaskKey("/w/foreman"); key != "task:foreman-1" {
		t.Errorf("a repo's task after two with no repo: %s", key)
	}
	if err := runDispatch([]string{"--no-worktree", "do it"}); err == nil {
		t.Error("--no-worktree took a task with no repo")
	}

	for _, dir := range []string{want, "/gone/task-1"} {
		if got := repoOf("task:1", dir); got != "" {
			t.Errorf("repoOf(task:1, %s) = %q", dir, got)
		}
	}
	if got := shortKey(queueItem{Key: "task:1"}); got != "task 1" {
		t.Errorf("shortKey = %q", got)
	}
}

// A task with no repo starts on Claude until an agent is picked for it, whatever pi is to the repos.
func TestNoRepoAgentIsClaude(t *testing.T) {
	if got := defaultAgent(State{AgentByRepo: map[string]string{}}, ""); got != agentClaude {
		t.Errorf("defaultAgent with no repo = %q, want %q", got, agentClaude)
	}
	if got := defaultAgent(State{AgentByRepo: map[string]string{"": agentPi}}, ""); got != agentPi {
		t.Errorf("a picked agent for no repo: %q", got)
	}
}

// A task with no repo is a cleanup candidate once its session closes; cleaning it up removes its
// scratch directory with whatever the agent left there.
func TestCleanupRemovesATasksScratchDirectory(t *testing.T) {
	useFakeRex(t)
	tempState(t)
	dir := taskDir("task:1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := checkCleanup(context.Background(), nil, "task:1", dir, true); ok || err != nil {
		t.Errorf("open session: candidate %t, %v", ok, err)
	}
	c, ok, err := checkCleanup(context.Background(), nil, "task:1", dir, false)
	if !ok || err != nil || c.Reason != "session closed" || !c.Scratch || c.Main || c.Dirty {
		t.Fatalf("closed session: %+v, %t, %v", c, ok, err)
	}
	if got := removeCandidates([]cleanupCandidate{c}); got.removed != 1 || len(got.errs) != 0 || exists(dir) {
		t.Errorf("removed %+v; the directory is there: %t", got, exists(dir))
	}
}

// The task form offers no repo after the repos; picking it dispatches a task with no repo and
// remembers the agent for it.
func TestTaskFormWithoutARepo(t *testing.T) {
	tempState(t)
	m := &model{
		state:    State{RepoByProject: map[string]string{}, AgentByRepo: map[string]string{}},
		repos:    []string{"/r/one", "/r/two"},
		details:  map[string][]string{},
		detailOK: map[string]bool{},
		agents:   map[string]string{},
	}
	m.boardInput(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	f := m.form
	f.field = fieldRepo
	for range 3 {
		m.formInput(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	if m.formRepo() != "" {
		t.Fatalf("the last choice is %q, want no repo", m.formRepo())
	}
	f.field = fieldContext
	m.paste("What is x?")
	if form := strings.Join(m.renderForm(100, 40), "\n"); !strings.Contains(form, "no repo") {
		t.Errorf("form:\n%s", form)
	}
	// The worktree option means nothing with no repo: it isn't shown, and tab passes it by.
	if form := strings.Join(m.renderForm(100, 40), "\n"); strings.Contains(form, "git worktree") {
		t.Errorf("the form offers a worktree with no repo:\n%s", form)
	}
	f.field = fieldAgent
	m.formInput(tea.KeyPressMsg{Code: tea.KeyTab})
	if f.field != fieldPoteto {
		t.Errorf("tab from the agent went to field %d, want poteto-mode %d", f.field, fieldPoteto)
	}
	m.formInput(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if f.field != fieldAgent {
		t.Errorf("shift+tab from poteto-mode went to field %d, want the agent", f.field)
	}
	f.field = fieldContext
	// Typing filters to it as well.
	f.repoQuery, f.repo = "no re", 0
	if choices := m.repoChoices(); len(choices) != 1 || choices[0] != "" {
		t.Errorf("no re filters to %q", choices)
	}
	if cmd := m.formInput(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil || !f.busy {
		t.Fatal("the task didn't dispatch")
	}
	if a, ok := loadState().AgentByRepo[""]; !ok || a == "" {
		t.Errorf("the agent isn't remembered for no repo: %+v", loadState().AgentByRepo)
	}
	// A ticket's form has no such choice.
	m.form = &form{row: issueRow(Issue{Key: "FLD-1"})}
	if choices := m.repoChoices(); len(choices) != 2 {
		t.Errorf("a ticket's repos: %q", choices)
	}
}

// The palette offers no repo after the repos, with the agent a task with no repo starts on, and
// dispatches without --repo.
func TestPaletteTaskWithoutARepo(t *testing.T) {
	h := newRexHost(t)
	paletteRepos(t, h)
	if _, err := updateState(func(s *State) { s.AgentByRepo[""] = agentAuto }); err != nil {
		t.Fatal(err)
	}
	h.answers = []func(*lua.LTable) (lua.LValue, string){
		func(spec *lua.LTable) (lua.LValue, string) {
			got := items(spec)
			if len(got) != 3 || got[2] != "none|No repo|" {
				t.Errorf("repos offered: %q, want No repo last", got)
			}
			return lua.LString("none"), ""
		},
		func(spec *lua.LTable) (lua.LValue, string) {
			if got := items(spec); len(got) == 0 || got[0] != agentAuto+"|"+agentAuto+"|" {
				t.Errorf("agents offered: %q, want %s first", got, agentAuto)
			}
			if title := spec.RawGetString("title").String(); title != "Agent, with no repo" {
				t.Errorf("the agent prompt is titled %q", title)
			}
			return lua.LString(agentClaude), ""
		},
		answer("what is x?"),
	}
	if err := h.do("foreman_task", map[string]string{"block_id": "block:1"}); err != nil {
		t.Fatal(err)
	}
	if len(h.started) != 1 {
		t.Fatalf("started %d commands, want the dispatch", len(h.started))
	}
	got := h.started[0]
	if want := `foreman dispatch --notify --agent "$FOREMAN_AGENT" -- "$FOREMAN_TASK"`; len(got.argv) != 3 || got.argv[2] != want {
		t.Errorf("started %q", got.argv)
	}
	a, err := parseDispatchArgs([]string{"--notify", "--agent", got.env["FOREMAN_AGENT"], "--", got.env["FOREMAN_TASK"]}, nil)
	if err != nil || a.repo != "" || a.text != "what is x?" {
		t.Errorf("foreman dispatch reads %+v, %v", a, err)
	}
}
