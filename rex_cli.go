package main

// rexCLI is Rex through its command line: the only place foreman runs `rex`.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type rexCLI struct{}

// attachLua attaches the script to a session: without it, listing another session's blocks fails
// ("stream is not attached to session") and the session looks as if no agent were running there.
const attachLua = `
local function attach(id)
  if rex.session.attach then pcall(rex.session.attach, { session_id = id }) end
end
`

// sessionsScript returns every session with its panes: each one's label, whether it is placed, the
// records its programs report over OSC 7501 (pi and Claude Code report working, blocked, done…)
// and the program it runs with its directory.
const sessionsScript = attachLua + `
local out = {}
local list = rex.call("session.list")
for _, s in ipairs((list or {}).sessions or {}) do
  attach(s.session_id)
  local panes = {}
  local blocks = rex.call("session.list_blocks", {session_id = s.session_id})
  for _, b in ipairs((blocks or {}).blocks or {}) do
    local pane = {id = b.block_id, label = b.label or "", placed = b.placement == "placed", records = {}}
    local ok, st = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "program_status"})
    if ok and st then
      for _, r in ipairs(st.records or {}) do
        table.insert(pane.records, {id = r.id or "", state = r.state or "", kind = r.kind or "", msg = r.msg or "", at = r.updated_at or ""})
      end
    end
    local okp, p = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "process"})
    if okp and p and p.child then
      pane.cmd, pane.dir = p.child.argv0 or "", p.child.cwd or ""
    end
    table.insert(panes, pane)
  end
  table.insert(out, {id = s.session_id, label = s.label, panes = panes})
end
return out
`

func (rexCLI) sessions() ([]rexSession, error) {
	// --all: otherwise rex do hears only the session REX_SESSION names, and listing another
	// session's blocks fails.
	out, err := sh("", "rex", "do", "--all", "-e", sessionsScript, "--json")
	if err != nil {
		return nil, err
	}
	return decodeSessions(out)
}

// decodeSessions reads what sessionsScript returns.
func decodeSessions(out string) ([]rexSession, error) {
	type pane struct {
		ID      string          `json:"id"`
		Label   string          `json:"label"`
		Placed  bool            `json:"placed"`
		Records json.RawMessage `json:"records"`
		rexProc
	}
	var raw []struct {
		ID    string          `json:"id"`
		Label string          `json:"label"`
		Panes json.RawMessage `json:"panes"`
	}
	if strings.TrimSpace(out) == "{}" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	sessions := make([]rexSession, 0, len(raw))
	for _, r := range raw {
		s := rexSession{ID: r.ID, Label: r.Label}
		for _, p := range luaList[pane](r.Panes) {
			rp := rexPane{ID: p.ID, Label: p.Label, Placed: p.Placed, Proc: p.rexProc}
			for _, w := range luaList[wireRecord](p.Records) {
				rp.Records = append(rp.Records, w.record())
			}
			s.Panes = append(s.Panes, rp)
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func (rexCLI) newSession(label, cwd, window string, cmd []string) (id, block string, err error) {
	// `rex new` starts the session in the background; the app stays where it is.
	args := append([]string{"new", label, "--json", "--cwd", cwd, "--window", window, "--"}, cmd...)
	out, err := sh("", "rex", args...)
	if err != nil {
		return "", "", err
	}
	// The ID, not the label: an agent may rename its session to describe the work.
	var made struct {
		ID      string `json:"session_id"`
		Windows []struct {
			Blocks []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if json.Unmarshal([]byte(out), &made) != nil || made.ID == "" {
		return label, "", nil
	}
	if len(made.Windows) > 0 && len(made.Windows[0].Blocks) > 0 {
		block = made.Windows[0].Blocks[0]
	}
	return made.ID, block, nil
}

// call runs a method of Rex's API with args as its JSON object, and decodes the result into out
// unless it is nil.
func (rexCLI) call(method string, args map[string]any, out any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := sh("", "rex", "api", "call", method, string(b))
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal([]byte(res), out)
}

func (c rexCLI) createSession(label, window string, layout rexLayout) (string, error) {
	var made struct {
		ID string `json:"session_id"`
	}
	err := c.call("session.create", map[string]any{
		"label":           label,
		"initial_windows": []map[string]any{{"window_label": window, "layout": layout}},
	}, &made)
	if err == nil && made.ID == "" {
		err = fmt.Errorf("Rex made the session %s without saying its ID", label)
	}
	return made.ID, err
}

func (rexCLI) kill(session string) error {
	_, err := sh("", "rex", "kill", session)
	return err
}

func (rexCLI) move(session, after string) error {
	_, err := sh("", "rex", "session", "move", session, "--after", after)
	return err
}

// rexClient is a client "rex client ls --json" lists.
type rexClient struct {
	ID   string `json:"client_id"`
	Info struct {
		Kind string `json:"kind"`
	} `json:"info"`
	Principal struct {
		Transport string `json:"transport"`
	} `json:"principal"`
	ConnectedAt time.Time `json:"connected_at"`
}

// pickApp chooses the Rex app to switch sessions in. Rex picks one only when exactly one app is
// connected, so with a phone connected too, prefer the app on this machine (a unix socket), then
// the latest to connect.
func pickApp(clients []rexClient) (string, error) {
	var best *rexClient
	for i := range clients {
		c := &clients[i]
		if c.Info.Kind != "app" {
			continue
		}
		if best == nil {
			best = c
			continue
		}
		local, bestLocal := c.Principal.Transport == "unix", best.Principal.Transport == "unix"
		if local != bestLocal {
			if local {
				best = c
			}
			continue
		}
		if c.ConnectedAt.After(best.ConnectedAt) {
			best = c
		}
	}
	if best == nil {
		return "", fmt.Errorf("no Rex app is connected")
	}
	return best.ID, nil
}

func (rexCLI) show(session string) error {
	out, err := sh("", "rex", "client", "ls", "--json")
	if err != nil {
		return err
	}
	var data struct {
		Clients []rexClient `json:"clients"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return err
	}
	client, err := pickApp(data.Clients)
	if err != nil {
		return err
	}
	script := fmt.Sprintf(`
for _, s in ipairs(rex.call("session.list").sessions or {}) do
  if s.session_id == %[1]s or s.label == %[1]s then
    rex.client.queue("session.select", {session_id = s.session_id})
    return s.session_id
  end
end
error("no session " .. %[1]s, 0)
`, strconv.Quote(session))
	_, err = sh("", "rex", "-C", client, "do", "-e", script)
	return err
}

func (rexCLI) capture(session, block string, unwrap bool) (string, error) {
	args := []string{"capture", "-s", session}
	if block != "" {
		args = append(args, "-b", block)
	}
	args = append(args, "--trim")
	if unwrap {
		args = append(args, "--unwrap")
	}
	return sh("", "rex", args...)
}

func (rexCLI) send(session, block, text string) error {
	// After --: a reply that starts with a dash isn't a flag.
	_, err := sh("", "rex", "send", "-s", session, "-b", block, "--", text)
	return err
}

func (rexCLI) press(session, block, key string) error {
	_, err := sh("", "rex", "send-key", "-s", session, "-b", block, key)
	return err
}

// eventsScript prints a line of JSON for each thing an agent's row shows that changed, in any
// session: a status with the record itself, so it needs no reading back, and what closed or was
// renamed. What only a listing can show (a new session or pane, a program that exited) is a sync.
const eventsScript = `
local function say(t) print(rex.json.encode(t)) end
rex.on("terminal.program_status_changed", function(ctx, ev)
  local r = ev.record or {}
  say{e = "status", session = ev.session_id, block = ev.block_id, id = ev.id or "",
      state = r.state or "", kind = r.kind or "", msg = r.msg or "", at = r.updated_at or ""}
end)
rex.on("terminal.program_status_removed", function(ctx, ev)
  say{e = "removed", session = ev.session_id, block = ev.block_id, ids = ev.ids or {}}
end)
rex.on("block_closed", function(ctx, ev) say{e = "closed", session = ev.session_id, block = ev.block_id} end)
rex.on("session_destroyed", function(ctx, ev) say{e = "gone", session = ev.session_id} end)
rex.on("session_label_changed", function(ctx, ev) say{e = "renamed", session = ev.session_id, label = ev.label} end)
for _, name in ipairs({"session_created", "block_created", "terminal.child_exited"}) do
  rex.on(name, function() say{e = "sync"} end)
end
say{e = "listening"}
`

func (rexCLI) events(ctx context.Context, each func(rexEvent)) error {
	cmd := exec.CommandContext(ctx, "rex", "do", "--all", "-e", eventsScript)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lines := bufio.NewScanner(out)
	for lines.Scan() {
		var ev rexEvent
		// Rex's own lines about the script aren't JSON.
		if json.Unmarshal(lines.Bytes(), &ev) == nil && ev.Type != "" {
			each(ev)
		}
	}
	return cmd.Wait()
}
