package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// attachLua attaches the script to a session: without it, listing another session's blocks fails
// ("stream is not attached to session") and the session looks as if no agent were running there.
const attachLua = `
local function attach(id)
  if rex.session.attach then pcall(rex.session.attach, { session_id = id }) end
end
`

// agentSessionsScript returns every session with the states its terminals' programs report over
// OSC 7501 (pi and Claude Code report working, blocked, done…).
const agentSessionsScript = attachLua + `
local out = {}
local list = rex.call("session.list")
for _, s in ipairs((list or {}).sessions or {}) do
  attach(s.session_id)
  local states = {}
  local blocks = rex.call("session.list_blocks", {session_id = s.session_id})
  for _, b in ipairs((blocks or {}).blocks or {}) do
    local ok, st = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "program_status"})
    if ok and st then
      for _, r in ipairs(st.records or {}) do table.insert(states, r.state) end
    end
  end
  table.insert(out, {id = s.session_id, label = s.label, states = states})
end
return out
`

// A rexSession is an open Rex session and its most urgent reported state.
type rexSession struct {
	ID, Label, State string
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
	}
	if strings.TrimSpace(out) == "{}" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	sessions := make([]rexSession, 0, len(raw))
	for _, r := range raw {
		var list []string
		_ = json.Unmarshal(r.States, &list)
		sessions = append(sessions, rexSession{r.ID, r.Label, mostUrgent(list)})
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
	if isTask(key) {
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
