package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// rexHost is the rex table rex/foreman.lua sees, in the Lua Rex itself embeds: its commands are
// answered by run, its palette by the answers in order, and what it asked and started is kept.
type rexHost struct {
	t       *testing.T
	L       *lua.LState
	actions map[string]*lua.LTable
	// run answers rex.exec: what the command printed, its stderr and whether it exited with 0.
	run     func(argv []string, env map[string]string) (stdout, stderr string, ok bool)
	answers []func(spec *lua.LTable) (lua.LValue, string)
	started []startedCommand
	cwd     string // the directory of the focused pane's program
	paneAsk int    // how many times the pane was asked for it
}

type startedCommand struct {
	argv   []string
	env    map[string]string
	detach bool
}

// noAnswer is why a prompt gave nothing: cancelled, timeout, busy…
func noAnswer(why string) func(*lua.LTable) (lua.LValue, string) {
	return func(*lua.LTable) (lua.LValue, string) { return lua.LNil, why }
}

func answer(text string) func(*lua.LTable) (lua.LValue, string) {
	return func(*lua.LTable) (lua.LValue, string) { return lua.LString(text), "" }
}

func stringsOf(t *lua.LTable) []string {
	var out []string
	t.ForEach(func(_, v lua.LValue) { out = append(out, v.String()) })
	return out
}

func stringMap(v lua.LValue) map[string]string {
	out := map[string]string{}
	if t, ok := v.(*lua.LTable); ok {
		t.ForEach(func(k, v lua.LValue) { out[k.String()] = v.String() })
	}
	return out
}

func toLua(L *lua.LState, v any) lua.LValue {
	switch v := v.(type) {
	case map[string]any:
		t := L.NewTable()
		for k, x := range v {
			t.RawSetString(k, toLua(L, x))
		}
		return t
	case []any:
		t := L.NewTable()
		for _, x := range v {
			t.Append(toLua(L, x))
		}
		return t
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case bool:
		return lua.LBool(v)
	}
	return lua.LNil
}

// newRexHost loads rex/foreman.lua against a fake rex, as init.lua's require does.
func newRexHost(t *testing.T) *rexHost {
	t.Helper()
	t.Setenv("SHELL", "/bin/login-shell")
	L := lua.NewState()
	t.Cleanup(L.Close)
	h := &rexHost{t: t, L: L, actions: map[string]*lua.LTable{}}
	fn := func(f func(L *lua.LState) int) *lua.LFunction { return L.NewFunction(f) }
	table := func(fields map[string]lua.LValue) *lua.LTable {
		t := L.NewTable()
		for k, v := range fields {
			t.RawSetString(k, v)
		}
		return t
	}
	prompt := func(L *lua.LState) int {
		if len(h.answers) == 0 {
			t.Errorf("asked %q with no answer left", L.CheckTable(1).RawGetString("title"))
			L.Push(lua.LNil)
			L.Push(lua.LString("cancelled"))
			return 2
		}
		next := h.answers[0]
		h.answers = h.answers[1:]
		v, why := next(L.CheckTable(1))
		L.Push(v)
		if v == lua.LNil {
			L.Push(lua.LString(why))
			return 2
		}
		return 1
	}
	// rex.exec is called, and rex.exec.start is a field of it.
	exec := table(map[string]lua.LValue{"start": fn(func(L *lua.LState) int {
		opts := L.OptTable(2, L.NewTable())
		h.started = append(h.started, startedCommand{stringsOf(L.CheckTable(1)), stringMap(opts.RawGetString("env")), lua.LVAsBool(opts.RawGetString("detach"))})
		L.Push(L.NewTable())
		return 1
	})})
	L.SetMetatable(exec, table(map[string]lua.LValue{"__call": fn(func(L *lua.LState) int {
		stdout, stderr, ok := h.run(stringsOf(L.CheckTable(2)), stringMap(L.OptTable(3, L.NewTable()).RawGetString("env")))
		L.Push(table(map[string]lua.LValue{"stdout": lua.LString(stdout), "stderr": lua.LString(stderr), "ok": lua.LBool(ok)}))
		return 1
	})}))
	L.SetGlobal("rex", table(map[string]lua.LValue{
		"exec": exec,
		"action": fn(func(L *lua.LState) int {
			spec := L.CheckTable(1)
			h.actions[spec.RawGetString("name").String()] = spec
			return 0
		}),
		"ui": table(map[string]lua.LValue{"pick": fn(prompt), "input": fn(prompt)}),
		"json": table(map[string]lua.LValue{"decode": fn(func(L *lua.LState) int {
			var v any
			if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
				L.RaiseError("%s", err.Error())
			}
			L.Push(toLua(L, v))
			return 1
		})}),
		"block": table(map[string]lua.LValue{"call": fn(func(L *lua.LState) int {
			h.paneAsk++
			L.Push(table(map[string]lua.LValue{"foreground": table(map[string]lua.LValue{"cwd": lua.LString(h.cwd)})}))
			return 1
		})}),
	}))
	if err := L.DoFile(filepath.Join("rex", "foreman.lua")); err != nil {
		t.Fatal(err)
	}
	return h
}

// do runs an action as a key in the focused pane would.
func (h *rexHost) do(name string, ctx map[string]string) error {
	spec, ok := h.actions[name]
	if !ok {
		h.t.Fatalf("no action %s", name)
	}
	tbl := func(m map[string]string) *lua.LTable {
		t := h.L.NewTable()
		for k, v := range m {
			t.RawSetString(k, lua.LString(v))
		}
		return t
	}
	return h.L.CallByParam(lua.P{Fn: spec.RawGetString("run"), NRet: 1, Protect: true}, tbl(ctx), h.L.NewTable())
}

// items are a pick's rows, each as "id|title|detail".
func items(spec *lua.LTable) []string {
	var out []string
	if list, ok := spec.RawGetString("items").(*lua.LTable); ok {
		list.ForEach(func(_, v lua.LValue) {
			it := v.(*lua.LTable)
			detail := ""
			if d := it.RawGetString("detail"); d != lua.LNil {
				detail = d.String()
			}
			out = append(out, it.RawGetString("id").String()+"|"+it.RawGetString("title").String()+"|"+detail)
		})
	}
	return out
}

// paletteRepos puts two repos under a root of their own, and a worktree of the second, and has
// the fake rex answer `foreman repos` with what the command itself prints.
func paletteRepos(t *testing.T, h *rexHost) (app, tool, worktree string) {
	t.Helper()
	tempState(t)
	root := t.TempDir()
	old := repoRoots
	repoRoots = []string{root}
	t.Cleanup(func() { repoRoots = old })
	app, tool, worktree = filepath.Join(root, "app"), filepath.Join(root, "tool"), filepath.Join(root, "tool-task-1")
	for _, r := range []string{app, tool} {
		git(t, root, "init", "-q", "-b", "main", r)
		git(t, r, "commit", "-q", "--allow-empty", "-m", "base")
	}
	git(t, tool, "worktree", "add", "-q", worktree)
	h.run = func(argv []string, env map[string]string) (string, string, bool) {
		if len(argv) != 3 || argv[0] != "/bin/login-shell" || argv[1] != "-lc" || argv[2] != `foreman repos --json --in "$FOREMAN_DIR"` {
			t.Errorf("rex.exec ran %q", argv)
		}
		var out bytes.Buffer
		if err := runRepos([]string{"--json", "--in", env["FOREMAN_DIR"]}, &out); err != nil {
			return "", err.Error(), false
		}
		return out.String(), "", true
	}
	return app, tool, worktree
}

// The palette's task: the repo of the focused pane comes first, a worktree counting as its repo,
// then that repo's usual agent, and what I type reaches foreman dispatch as I typed it.
func TestPaletteTask(t *testing.T) {
	h := newRexHost(t)
	app, tool, worktree := paletteRepos(t, h)
	if _, err := updateState(func(s *State) { s.AgentByRepo[tool] = agentAuto }); err != nil {
		t.Fatal(err)
	}
	h.cwd = filepath.Join(worktree, "sub")
	if err := os.Mkdir(h.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	task := `fix the "quoted" $HOME 'thing'`
	h.answers = []func(*lua.LTable) (lua.LValue, string){
		func(spec *lua.LTable) (lua.LValue, string) {
			if got, want := items(spec), []string{tool + "|tool|here", app + "|app|"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("repos offered:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			return lua.LString(tool), ""
		},
		func(spec *lua.LTable) (lua.LValue, string) {
			got := items(spec)
			if len(got) != len(agents) || got[0] != agentAuto+"|"+agentAuto+"|" {
				t.Errorf("agents offered: %q, want %s first of %d", got, agentAuto, len(agents))
			}
			if title := spec.RawGetString("title").String(); title != "Agent in tool" {
				t.Errorf("the agent prompt is titled %q", title)
			}
			return lua.LString(agentPi), ""
		},
		answer("  " + task + " \n"),
	}
	if err := h.do("foreman_task", map[string]string{"block_id": "block:1"}); err != nil {
		t.Fatal(err)
	}
	if len(h.started) != 1 {
		t.Fatalf("started %d commands, want the dispatch", len(h.started))
	}
	got := h.started[0]
	if want := `foreman dispatch --notify --repo "$FOREMAN_REPO" --agent "$FOREMAN_AGENT" -- "$FOREMAN_TASK"`; len(got.argv) != 3 || got.argv[2] != want || !got.detach {
		t.Errorf("started %q, detached %t", got.argv, got.detach)
	}
	// What the shell hands foreman once it has put the variables in.
	a, err := parseDispatchArgs([]string{"--notify", "--repo", got.env["FOREMAN_REPO"], "--agent", got.env["FOREMAN_AGENT"], "--", got.env["FOREMAN_TASK"]}, nil)
	if err != nil || a.repo != tool || a.text != task || !a.notify {
		t.Errorf("foreman dispatch reads %+v, %v", a, err)
	}
	if agent, err := resolveAgent(a.agent); err != nil || agent != agentPi {
		t.Errorf("the agent picked reads as %q, %v", agent, err)
	}
}

// Closing a prompt ends the action quietly, with nothing dispatched; a prompt Rex couldn't show
// fails it with the reason.
func TestPaletteTaskUnanswered(t *testing.T) {
	// A nil answer picks the repo.
	for _, tc := range []struct {
		name    string
		answers []func(*lua.LTable) (lua.LValue, string)
		wantErr string
	}{
		{"no repo", []func(*lua.LTable) (lua.LValue, string){noAnswer("cancelled")}, ""},
		{"no agent", []func(*lua.LTable) (lua.LValue, string){nil, noAnswer("timeout")}, ""},
		{"no task", []func(*lua.LTable) (lua.LValue, string){nil, answer(agentPi), noAnswer("cancelled")}, ""},
		{"a blank task", []func(*lua.LTable) (lua.LValue, string){nil, answer(agentPi), answer("  ")}, ""},
		{"no app to ask", []func(*lua.LTable) (lua.LValue, string){noAnswer("no_client")}, "New Foreman Task: no_client"},
	} {
		h := newRexHost(t)
		_, tool, _ := paletteRepos(t, h)
		for n, a := range tc.answers {
			if a == nil {
				tc.answers[n] = answer(tool)
			}
		}
		h.answers = tc.answers
		err := h.do("foreman_task", map[string]string{"block_id": "block:1"})
		if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.wantErr)
		}
		if len(h.started) != 0 || len(h.answers) != 0 {
			t.Errorf("%s: started %d commands, %d answers unused", tc.name, len(h.started), len(h.answers))
		}
	}
}

// A remote host's pane isn't asked for its directory, which is that host's; and a foreman that
// fails says why in the action's error.
func TestPaletteTaskRemoteAndFailure(t *testing.T) {
	h := newRexHost(t)
	paletteRepos(t, h)
	h.answers = []func(*lua.LTable) (lua.LValue, string){func(spec *lua.LTable) (lua.LValue, string) {
		for _, it := range items(spec) {
			if strings.HasSuffix(it, "|here") {
				t.Errorf("a remote pane put %q first", it)
			}
		}
		return lua.LNil, "cancelled"
	}}
	if err := h.do("foreman_task", map[string]string{"block_id": "block:1", "server": "devbox"}); err != nil {
		t.Fatal(err)
	}
	if h.paneAsk != 0 {
		t.Error("asked a remote pane for its directory")
	}

	h = newRexHost(t)
	h.run = func([]string, map[string]string) (string, string, bool) {
		return "", "foreman: command not found\n", false
	}
	if err := h.do("foreman_task", nil); err == nil || !strings.Contains(err.Error(), "foreman: command not found") {
		t.Errorf("a failing foreman: %v", err)
	}
}

// The palette's page: foreman page runs for the focused pane, which it is told as REX_BLOCK, in
// the directory of the pane's program; what it fails with is the action's error.
func TestPalettePage(t *testing.T) {
	h := newRexHost(t)
	h.cwd = "/w/app-fld-1"
	var ran []string
	var env map[string]string
	h.run = func(argv []string, e map[string]string) (string, string, bool) {
		ran, env = argv, e
		return "✓ Opened https://github.com/o/app/pull/7\n", "", true
	}
	if err := h.do("foreman_page", map[string]string{"block_id": "block:2"}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 3 || ran[0] != "/bin/login-shell" || ran[2] != `foreman page --in "$FOREMAN_DIR"` || env["FOREMAN_DIR"] != "/w/app-fld-1" || env["REX_BLOCK"] != "block:2" {
		t.Errorf("ran %q with %q", ran, env)
	}
	// What the shell hands foreman once it has put the variable in.
	fs := newFlags("page")
	in := fs.String("in", "", "")
	if _, err := parseFlags(fs, []string{"--in", env["FOREMAN_DIR"]}, 0); err != nil || *in != "/w/app-fld-1" {
		t.Errorf("foreman page reads --in %q, %v", *in, err)
	}

	h.run = func([]string, map[string]string) (string, string, bool) {
		return "", "task:app-3 has no PR yet, and no ticket\n", false
	}
	if err := h.do("foreman_page", map[string]string{"block_id": "block:2"}); err == nil || !strings.Contains(err.Error(), "task:app-3 has no PR yet, and no ticket") {
		t.Errorf("a pane with no page: %v", err)
	}

	// A remote host's pane, or no pane at all, runs nothing.
	ran = nil
	h.run = func(argv []string, _ map[string]string) (string, string, bool) { ran = argv; return "", "", true }
	for _, ctx := range []map[string]string{{"block_id": "block:2", "server": "devbox"}, nil} {
		if err := h.do("foreman_page", ctx); err == nil || !strings.Contains(err.Error(), "Show Foreman Page: ") || ran != nil {
			t.Errorf("ctx %v: err %v, ran %q", ctx, err, ran)
		}
	}
}
