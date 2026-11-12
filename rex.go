package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// attachLua attaches the script to a session: without it, listing another session's blocks fails
// ("stream is not attached to session") and the session looks as if no agent were running there.
const attachLua = `
local function attach(id)
  if rex.session.attach then pcall(rex.session.attach, { session_id = id }) end
end
`

// agentSessionsScript returns every session with the states its terminals' programs report over
// OSC 7501 (pi and Claude Code report working, blocked, done…), and each terminal's program with
// its directory.
const agentSessionsScript = attachLua + `
local out = {}
local list = rex.call("session.list")
for _, s in ipairs((list or {}).sessions or {}) do
  attach(s.session_id)
  local states, msgs, procs = {}, {}, {}
  local blocks = rex.call("session.list_blocks", {session_id = s.session_id})
  for _, b in ipairs((blocks or {}).blocks or {}) do
    local ok, st = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "program_status"})
    if ok and st then
      for _, r in ipairs(st.records or {}) do
        table.insert(states, r.state)
        table.insert(msgs, r.msg or "")
      end
    end
    local okp, p = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "process"})
    if okp and p and p.child then
      table.insert(procs, {cmd = p.child.argv0 or "", dir = p.child.cwd or ""})
    end
  end
  table.insert(out, {id = s.session_id, label = s.label, states = states, msgs = msgs, procs = procs})
end
return out
`

// A rexSession is an open Rex session, its most urgent reported state, the programs its terminals
// run, and what the program in that state says it is doing, when it says.
type rexSession struct {
	ID, Label, State string
	Procs            []rexProc
	Msg              string
}

// A rexProc is a terminal's program and the directory it runs in.
type rexProc struct {
	Cmd string `json:"cmd"`
	Dir string `json:"dir"`
}

// runsClaudeIn is whether one of the session's terminals runs Claude Code in dir.
func (s rexSession) runsClaudeIn(dir string) bool {
	for _, p := range s.Procs {
		if filepath.Base(p.Cmd) == "claude" && filepath.Clean(p.Dir) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// agentSessions lists the open Rex sessions with their most urgent reported state: blocked, error,
// working, done or idle, or "open" when their programs report nothing.
func agentSessions() ([]rexSession, error) {
	// --all: otherwise rex do hears only the session REX_SESSION names, and listing another
	// session's blocks fails.
	out, err := sh("", "rex", "do", "--all", "-e", agentSessionsScript, "--json")
	if err != nil {
		return nil, err
	}
	// A Lua table with no entries encodes as {} rather than [], so decode the list and states loosely.
	var raw []struct {
		ID     string          `json:"id"`
		Label  string          `json:"label"`
		States json.RawMessage `json:"states"`
		Msgs   json.RawMessage `json:"msgs"`
		Procs  json.RawMessage `json:"procs"`
	}
	if strings.TrimSpace(out) == "{}" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	sessions := make([]rexSession, 0, len(raw))
	for _, r := range raw {
		var list, msgs []string
		var procs []rexProc
		_ = json.Unmarshal(r.States, &list)
		_ = json.Unmarshal(r.Msgs, &msgs)
		_ = json.Unmarshal(r.Procs, &procs)
		state := mostUrgent(list)
		msg := ""
		if n := slices.Index(list, state); n >= 0 && n < len(msgs) {
			msg = msgs[n]
		}
		sessions = append(sessions, rexSession{ID: r.ID, Label: r.Label, State: state, Procs: procs, Msg: msg})
	}
	return sessions, nil
}

// agentStates maps each Rex session's ID and label to its most urgent reported state. By both: the
// state file holds IDs, and labels for older dispatches.
func agentStates() (map[string]string, error) {
	sessions, err := agentSessions()
	if err != nil {
		return nil, err
	}
	return statesByName(sessions), nil
}

func statesByName(sessions []rexSession) map[string]string {
	states := map[string]string{}
	for _, s := range sessions {
		states[s.ID] = s.State
		states[s.Label] = s.State
	}
	return states
}

// findSession is the open session for a dispatched key: the recorded one, given by ID or label, else
// one whose label still starts with the key, as dispatch names it (fld-1704-…, review-repo-12-…).
// An agent that renamed a session recorded by label (older dispatches) keeps the key in it.
func findSession(sessions []rexSession, key, recorded string) (rexSession, bool) {
	for _, s := range sessions {
		if recorded != "" && (s.ID == recorded || s.Label == recorded) {
			return s, true
		}
	}
	k := slug(key)
	if isTask(key) || isFix(key) {
		k = keySlug(key)
	}
	for _, s := range sessions {
		for _, base := range []string{k, "review-" + k} {
			if s.Label == base || strings.HasPrefix(s.Label, base+"-") {
				return s, true
			}
		}
	}
	return rexSession{}, false
}

// killSession closes a Rex session, given by ID or label, and everything running in it.
func killSession(session string) error {
	_, err := sh("", "rex", "kill", session)
	return err
}

func mostUrgent(states []string) string {
	for _, want := range []string{"blocked", "error", "working", "done", "idle"} {
		for _, s := range states {
			if s == want {
				return want
			}
		}
	}
	return "open"
}

// switchTo shows the session, given by ID or label, in the Rex app, as cmd+s would.
func switchTo(label string) error {
	script := fmt.Sprintf(`
for _, s in ipairs(rex.call("session.list").sessions or {}) do
  if s.session_id == %[1]s or s.label == %[1]s then
    rex.client.queue("session.select", {session_id = s.session_id})
    return s.session_id
  end
end
error("no session " .. %[1]s, 0)
`, strconv.Quote(label))
	_, err := sh("", "rex", "do", "-e", script)
	return err
}

func freeSessionLabel(base string) (string, error) {
	out, err := sh("", "rex", "ls", "--json")
	if err != nil {
		return "", err
	}
	var data struct {
		Sessions []struct {
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, s := range data.Sessions {
		taken[s.Label] = true
	}
	label := base
	for n := 2; taken[label]; n++ {
		label = fmt.Sprintf("%s-%d", base, n)
	}
	return label, nil
}

// rexEventsScript prints a line whenever something an agent's row shows may have changed, in any
// session: a program's status, or a session or a pane opening, closing or being renamed.
const rexEventsScript = `
for _, name in ipairs({
  "terminal.program_status_changed", "terminal.program_status_removed", "terminal.child_exited",
  "session_created", "session_destroyed", "session_label_changed", "block_created", "block_closed",
}) do
  rex.on(name, function() print("event") end)
end
print("listening")
`

type (
	// rexEventMsg says the sessions changed: list them now rather than at the next tick.
	rexEventMsg struct{}
	// rexLiveMsg says whether Rex's events arrive. While they do, the sessions are listed on an
	// event and every sessionsFallback; without them, on every tick.
	rexLiveMsg bool
)

const (
	sessionsFallback = 15 * time.Second
	// A program that redraws its status line reports in bursts: one listing answers them all.
	rexEventSettle = 300 * time.Millisecond
	rexEventsRetry = 10 * time.Second
)

// watchRex follows Rex's events until ctx ends, sending a rexEventMsg after each burst and a
// rexLiveMsg as the stream starts and stops. A stream that ends is started again.
func watchRex(ctx context.Context, send func(tea.Msg)) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "rex", "do", "--all", "-e", rexEventsScript)
		out, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			var pending atomic.Bool
			lines := bufio.NewScanner(out)
			for lines.Scan() {
				if lines.Text() == "listening" {
					send(rexLiveMsg(true))
					continue
				}
				if pending.CompareAndSwap(false, true) {
					time.AfterFunc(rexEventSettle, func() {
						pending.Store(false)
						send(rexEventMsg{})
					})
				}
			}
			_ = cmd.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		send(rexLiveMsg(false))
		select {
		case <-ctx.Done():
		case <-time.After(rexEventsRetry):
		}
	}
}
