package main

// Rex as foreman sees it: the open sessions, their panes and the status each pane's program
// reports. An agent is one pane of its session, the one Rex made at dispatch, so its state is that
// pane's, whatever else runs beside it. Every call to Rex goes through rexAPI.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A rexRecord is one status a program reported over OSC 7501.
type rexRecord struct {
	ID    string // "" for the program's own record, a path for one of its workers'
	State string
	Kind  string // with blocked, what it waits for: permission, question or auth
	Msg   string
	At    time.Time
}

// A rexPane is a terminal of a session, with what its program reports and what it runs.
type rexPane struct {
	ID, Label string
	Placed    bool // in a window, rather than detached
	Records   []rexRecord
	Proc      rexProc
}

// A rexProc is a terminal's program and the directory it runs in.
type rexProc struct {
	Cmd string `json:"cmd"`
	Dir string `json:"dir"`
}

// A rexSession is an open Rex session. State, Kind, Msg and Block describe its agent's pane, once
// resolveAgents or on has picked it: the most urgent state the pane's program reports ("open" when
// it reports none), what a blocked one waits for, and what it says it is doing.
type rexSession struct {
	ID, Label string
	State     string
	Procs     []rexProc
	Msg       string
	Kind      string
	Block     string
	Panes     []rexPane
}

// runsClaudeIn is whether one of the session's terminals runs Claude Code in dir.
func (s rexSession) runsClaudeIn(dir string) bool {
	for _, p := range s.Procs {
		if p.runsClaudeIn(dir) {
			return true
		}
	}
	return false
}

func (p rexProc) runsClaudeIn(dir string) bool {
	return filepath.Base(p.Cmd) == "claude" && filepath.Clean(p.Dir) == filepath.Clean(dir)
}

// onClaudeIn is the session with the pane that runs Claude Code in dir as its agent's.
func (s rexSession) onClaudeIn(dir string) rexSession {
	for _, p := range s.Panes {
		if p.Proc.runsClaudeIn(dir) {
			return s.on(p.ID)
		}
	}
	return s
}

// agentPane is the pane the session's agent runs in: the recorded one while it is open, else the
// oldest placed pane whose program reports a status, else the oldest placed pane. Oldest, since a
// pane split beside the agent later (a shell, a fork of its conversation) isn't it; Rex's IDs
// sort by age.
func (s rexSession) agentPane(recorded string) string {
	var reporting, first string
	for _, p := range s.Panes {
		if p.ID == recorded && recorded != "" {
			return p.ID
		}
		if !p.Placed {
			continue
		}
		if len(p.Records) > 0 && (reporting == "" || p.ID < reporting) {
			reporting = p.ID
		}
		if first == "" || p.ID < first {
			first = p.ID
		}
	}
	return cmp.Or(reporting, first)
}

// on is the session with block as its agent's pane. A session listed without its panes stays as
// it is.
func (s rexSession) on(block string) rexSession {
	if len(s.Panes) == 0 {
		return s
	}
	s.Block, s.State, s.Kind, s.Msg = block, "open", "", ""
	n := slices.IndexFunc(s.Panes, func(p rexPane) bool { return p.ID == block })
	if n < 0 {
		s.Block = ""
		return s
	}
	var states []string
	for _, r := range s.Panes[n].Records {
		states = append(states, r.State)
	}
	s.State = mostUrgent(states)
	if r := slices.Index(states, s.State); r >= 0 {
		s.Kind, s.Msg = s.Panes[n].Records[r].Kind, s.Panes[n].Records[r].Msg
	}
	return s
}

// resolveAgents gives each session its agent's pane: the one recorded for it in panes, by session
// ID, else the one agentPane picks.
func resolveAgents(sessions []rexSession, panes map[string]string) []rexSession {
	out := make([]rexSession, len(sessions))
	for n, s := range sessions {
		s.Procs = nil
		for _, p := range s.Panes {
			if p.Proc != (rexProc{}) {
				s.Procs = append(s.Procs, p.Proc)
			}
		}
		out[n] = s.on(s.agentPane(panes[s.ID]))
		// A session with no pane left is still open.
		if out[n].State == "" {
			out[n].State = "open"
		}
	}
	return out
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

// A rexLayout is a window's panes as Rex's API takes them.
type rexLayout map[string]any

// paneLayout is one terminal running cmd in cwd.
func paneLayout(label, cwd string, cmd []string) rexLayout {
	return rexLayout{"block": map[string]any{
		"flavor":  "com.superlogical.terminal.shell",
		"label":   label,
		"options": map[string]any{"command": cmd, "cwd": cwd},
	}}
}

// rexAPI is everything foreman asks of Rex. Sessions are given by ID or label, panes by ID.
type rexAPI interface {
	// sessions lists the open sessions with their panes; no agent's pane is picked yet.
	sessions() ([]rexSession, error)
	// newSession starts cmd in a background session, and returns its ID and its pane's.
	newSession(label, cwd, window string, cmd []string) (id, block string, err error)
	// createSession makes a session with one window laid out as layout.
	createSession(label, window string, layout rexLayout) (id string, err error)
	kill(session string) error
	// show brings the session up in the Rex app, as cmd+s would.
	show(session string) error
	// capture is the pane's text, scrollback included; without a block, the focused pane's.
	capture(session, block string, unwrap bool) (string, error)
	// send types text into the pane; press presses a key there.
	send(session, block, text string) error
	press(session, block, key string) error
	// events calls each for every event until the stream ends or ctx does.
	events(ctx context.Context, each func(rexEvent)) error
}

// rex is the Rex foreman talks to; tests put a fake here.
var rex rexAPI = rexCLI{}

// A rexEvent is something that changed in Rex, as the listener reports it.
type rexEvent struct {
	// Type is listening (the stream started), status (a pane's program reported), removed (records
	// went), closed (a pane), gone (a session), renamed (a session), sync (something only a listing
	// shows: a new session or pane, a program that exited) or down (the stream ended).
	Type    string          `json:"e"`
	Session string          `json:"session"`
	Block   string          `json:"block"`
	Label   string          `json:"label"`
	IDs     json.RawMessage `json:"ids"`
	wireRecord
}

// wireRecord is a status record as Rex's JSON gives it.
type wireRecord struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Kind  string `json:"kind"`
	Msg   string `json:"msg"`
	At    string `json:"at"`
}

func (w wireRecord) record() rexRecord {
	at, _ := time.Parse(time.RFC3339Nano, w.At)
	return rexRecord{ID: w.ID, State: w.State, Kind: w.Kind, Msg: w.Msg, At: at}
}

// luaList decodes a list from a Lua script: a table with no entries encodes as {} rather than [],
// which decodes to none.
func luaList[T any](raw json.RawMessage) []T {
	var out []T
	_ = json.Unmarshal(raw, &out)
	return out
}

// A rexPicture is the open sessions as Rex's events last left them. It is live while the event
// stream runs and a listing has filled it; otherwise the sessions have to be listed.
type rexPicture struct {
	mu       sync.Mutex
	live     bool
	sessions []rexSession
}

// following is the picture this process keeps, when it follows Rex's events: the board and the queue.
var following atomic.Pointer[rexPicture]

func (p *rexPicture) set(sessions []rexSession, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions, p.live = sessions, live
}

// snapshot is a copy of the sessions, and whether the picture is live.
func (p *rexPicture) snapshot() ([]rexSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]rexSession, len(p.sessions))
	for n, s := range p.sessions {
		s.Panes = slices.Clone(s.Panes)
		for i := range s.Panes {
			s.Panes[i].Records = slices.Clone(s.Panes[i].Records)
		}
		out[n] = s
	}
	return out, p.live
}

// apply brings the picture up to date with an event. changed is whether anything an agent's row
// shows is different; resync is whether only a listing can tell what the event did.
func (p *rexPicture) apply(ev rexEvent) (changed, resync bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := slices.IndexFunc(p.sessions, func(s rexSession) bool { return s.ID == ev.Session })
	switch ev.Type {
	case "gone":
		if s >= 0 {
			p.sessions = slices.Delete(p.sessions, s, s+1)
		}
		return s >= 0, false
	case "renamed":
		if s < 0 {
			return false, true
		}
		changed = p.sessions[s].Label != ev.Label
		p.sessions[s].Label = ev.Label
		return changed, false
	case "closed", "status", "removed":
	default:
		return false, true
	}
	if s < 0 {
		// A session newer than the last listing; one already gone has nothing to close.
		return false, ev.Type == "status"
	}
	panes := p.sessions[s].Panes
	b := slices.IndexFunc(panes, func(p rexPane) bool { return p.ID == ev.Block })
	if b < 0 {
		return false, ev.Type == "status"
	}
	switch ev.Type {
	case "closed":
		p.sessions[s].Panes = slices.Delete(panes, b, b+1)
		return true, false
	case "removed":
		ids := luaList[string](ev.IDs)
		before := len(panes[b].Records)
		panes[b].Records = slices.DeleteFunc(panes[b].Records, func(r rexRecord) bool { return slices.Contains(ids, r.ID) })
		return len(panes[b].Records) != before, false
	}
	now := ev.record()
	r := slices.IndexFunc(panes[b].Records, func(r rexRecord) bool { return r.ID == now.ID })
	if r < 0 {
		panes[b].Records = append(panes[b].Records, now)
		return true, false
	}
	was := panes[b].Records[r]
	// An event from before the last listing, read after it: the listing is newer.
	if !now.At.IsZero() && now.At.Before(was.At) {
		return false, false
	}
	panes[b].Records[r] = now
	return was.State != now.State || was.Kind != now.Kind || was.Msg != now.Msg, false
}

const (
	// sessionsFallback is how often the picture is listed anew while events arrive, in case one was missed.
	sessionsFallback = 15 * time.Second
	// A program that redraws its status line reports in bursts: one redraw answers them all.
	rexEventSettle = 300 * time.Millisecond
	rexEventsRetry = 10 * time.Second
)

// rexEventMsg says the sessions changed: read them now rather than at the next tick.
type rexEventMsg struct{}

// followRex keeps this process's picture of Rex from its events until ctx ends, calling changed
// after each burst of them. A status report is applied as it arrives, with no call to Rex; a new
// session or pane takes a listing, as does the start of the stream. A stream that ends is started
// again, and until it is back the sessions are listed on every read.
func followRex(ctx context.Context, changed func()) {
	p := &rexPicture{}
	following.Store(p)
	defer following.Store(nil)
	events := make(chan rexEvent, 256)
	put := func(ev rexEvent) {
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}
	go func() {
		for ctx.Err() == nil {
			_ = rex.events(ctx, put)
			put(rexEvent{Type: "down"})
			select {
			case <-ctx.Done():
			case <-time.After(rexEventsRetry):
			}
		}
	}()
	var notify, resync <-chan time.Time
	up := false
	list := func() {
		resync = nil
		sessions, err := rex.sessions()
		p.set(sessions, err == nil && up)
		if notify == nil {
			notify = time.After(rexEventSettle)
		}
	}
	fallback := time.NewTicker(sessionsFallback)
	defer fallback.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			switch ev.Type {
			case "listening":
				up = true
				list()
			case "down":
				up = false
				p.set(nil, false)
			default:
				moved, unsure := p.apply(ev)
				if unsure && resync == nil {
					resync = time.After(rexEventSettle)
				}
				if moved && notify == nil {
					notify = time.After(rexEventSettle)
				}
			}
		case <-resync:
			list()
		case <-fallback.C:
			if up {
				list()
			}
		case <-notify:
			notify = nil
			changed()
		}
	}
}

// openSessions lists the open sessions: from the picture while this process follows Rex's events,
// else from Rex.
func openSessions() ([]rexSession, error) {
	if p := following.Load(); p != nil {
		if sessions, live := p.snapshot(); live {
			return sessions, nil
		}
	}
	return rex.sessions()
}

// agentSessions lists the open sessions, each with its agent's pane and that pane's state.
func agentSessions() ([]rexSession, error) {
	sessions, err := openSessions()
	if err != nil {
		return nil, err
	}
	return resolveAgents(sessions, loadState().Panes), nil
}

// agentStates maps each Rex session's ID and label to its agent's state. By both: the state file
// holds IDs, and labels for older dispatches.
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

// freeLabel is base, or base-2… when a session already has that label.
func freeLabel(base string, sessions []rexSession) string {
	taken := func(label string) bool {
		return slices.ContainsFunc(sessions, func(s rexSession) bool { return s.Label == label })
	}
	label := base
	for n := 2; taken(label); n++ {
		label = base + "-" + strconv.Itoa(n)
	}
	return label
}

// newSession starts cmd in a background Rex session named label, or label-2… when that is taken,
// and records its pane as the agent's. It returns the session's ID, to track it by, and its label.
func newSession(label, cwd, window string, cmd []string) (id, name string, err error) {
	sessions, err := openSessions()
	if err != nil {
		return "", "", err
	}
	name = freeLabel(label, sessions)
	id, block, err := rex.newSession(name, cwd, window, cmd)
	if err != nil {
		return "", "", err
	}
	if block != "" {
		_, _ = updateState(func(s *State) { recordPane(s, id, block, sessions) })
	}
	return id, name, nil
}

// recordPane notes block as the pane of the session's agent, and forgets the panes of sessions
// that are neither open nor anything's recorded session. Not by openness alone: a session another
// foreman started since open was listed isn't in it.
func recordPane(s *State, session, block string, open []rexSession) {
	keep := map[string]bool{s.Orchestrator: true}
	for _, o := range open {
		keep[o.ID] = true
	}
	for _, id := range s.Sessions {
		keep[id] = true
	}
	for _, id := range s.WatcherSessions {
		keep[id] = true
	}
	for id := range s.Panes {
		if !keep[id] {
			delete(s.Panes, id)
		}
	}
	s.Panes[session] = block
}

// sendToAgent types text into the agent's pane and presses enter, as if I had replied there.
func sendToAgent(session, block, text string) error {
	if block == "" {
		return fmt.Errorf("no pane in session %s", session)
	}
	if err := rex.send(session, block, text); err != nil {
		return err
	}
	// A separate key press: in one write the agent's editor takes the newline as pasted text.
	time.Sleep(sendSettle)
	return rex.press(session, block, "enter")
}

// sendSettle is the pause between typing a reply and pressing enter.
var sendSettle = 150 * time.Millisecond
