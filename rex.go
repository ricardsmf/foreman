package main

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// agentStatesScript returns, for every session, the states its terminals' programs report over
// OSC 7501 (pi and Claude Code report working, blocked, done…), keyed by session label.
const agentStatesScript = `
local out = {}
local list = rex.call("session.list")
for _, s in ipairs((list or {}).sessions or {}) do
  local states = {}
  local blocks = rex.call("session.list_blocks", {session_id = s.session_id})
  for _, b in ipairs((blocks or {}).blocks or {}) do
    local ok, st = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "program_status"})
    if ok and st then
      for _, r in ipairs(st.records or {}) do table.insert(states, r.state) end
    end
  end
  out[s.label] = states
end
return out
`

// agentStates maps each Rex session label to its most urgent reported state: blocked, error,
// working, done or idle, or "open" when its programs report nothing.
func agentStates() (map[string]string, error) {
	out, err := sh("", "rex", "do", "-e", agentStatesScript, "--json")
	if err != nil {
		return nil, err
	}
	// A Lua table with no entries encodes as {} rather than [], so decode each value loosely.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	states := map[string]string{}
	for label, v := range raw {
		var list []string
		_ = json.Unmarshal(v, &list)
		states[label] = mostUrgent(list)
	}
	return states, nil
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

// switchTo shows the session in the Rex app, as cmd+s would.
func switchTo(label string) error {
	script := fmt.Sprintf(`
for _, s in ipairs(rex.call("session.list").sessions or {}) do
  if s.label == %s then
    rex.client.queue("session.select", {session_id = s.session_id})
    return s.session_id
  end
end
error("no session " .. %s, 0)
`, strconv.Quote(label), strconv.Quote(label))
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
