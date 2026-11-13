package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRex is a Rex that holds sessions in memory and notes what it was asked to do.
type fakeRex struct {
	mu     sync.Mutex
	open   []rexSession
	lists  int
	calls  []string
	stream chan rexEvent
}

func (f *fakeRex) note(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeRex) sessions() ([]rexSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return slices.Clone(f.open), nil
}

func (f *fakeRex) listed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeRex) newSession(label, cwd, window string, cmd []string) (string, string, error) {
	f.note("new %s", label)
	return "session:" + label, "block:" + label, nil
}

func (f *fakeRex) createSession(label, window string, layout rexLayout) (string, error) {
	b, _ := json.Marshal(layout)
	f.note("create %s %s %s", label, window, b)
	return "session:" + label, nil
}

func (f *fakeRex) kill(session string) error { f.note("kill %s", session); return nil }
func (f *fakeRex) move(session, after string) error {
	f.note("move %s after %s", session, after)
	return nil
}
func (f *fakeRex) show(session string) error { f.note("show %s", session); return nil }
func (f *fakeRex) capture(session, block string, unwrap bool) (string, error) {
	f.note("capture %s %s", session, block)
	return "screen of " + block, nil
}
func (f *fakeRex) send(s, block, text string) error {
	f.note("send %s %s %s", s, block, text)
	return nil
}
func (f *fakeRex) press(s, block, key string) error {
	f.note("press %s %s %s", s, block, key)
	return nil
}

func (f *fakeRex) events(ctx context.Context, each func(rexEvent)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-f.stream:
			each(ev)
		}
	}
}

// useFakeRex puts a fake Rex and an empty state file in place for the test.
func useFakeRex(t *testing.T, open ...rexSession) *fakeRex {
	t.Helper()
	f := &fakeRex{open: open, stream: make(chan rexEvent)}
	oldRex, oldState, oldSettle := rex, stateFile, sendSettle
	rex, stateFile, sendSettle = f, filepath.Join(t.TempDir(), "state.json"), 0
	t.Cleanup(func() { rex, stateFile, sendSettle = oldRex, oldState, oldSettle })
	return f
}

func pane(id string, placed bool, records ...rexRecord) rexPane {
	return rexPane{ID: id, Label: "shell", Placed: placed, Records: records}
}

// An agent is its own pane: what a program split beside it reports isn't the agent's state.
func TestAgentStateIsItsPanes(t *testing.T) {
	s := rexSession{ID: "s1", Label: "fld-1-x", Panes: []rexPane{
		pane("block:1", true, rexRecord{State: "working", Msg: "Running the tests"}),
		pane("block:2", true, rexRecord{State: "blocked", Kind: "permission", Msg: "a fork asks"}),
	}}
	got := resolveAgents([]rexSession{s}, map[string]string{"s1": "block:1"})[0]
	if got.State != "working" || got.Block != "block:1" || got.Msg != "Running the tests" || got.Kind != "" {
		t.Errorf("recorded pane: got %s %q on %s (kind %q); want working on block:1", got.State, got.Msg, got.Block, got.Kind)
	}
	// Recorded on the fork instead, it is the fork that speaks.
	got = resolveAgents([]rexSession{s}, map[string]string{"s1": "block:2"})[0]
	if got.State != "blocked" || got.Kind != "permission" || got.Block != "block:2" {
		t.Errorf("fork recorded: got %s/%s on %s", got.State, got.Kind, got.Block)
	}
}

func TestAgentPaneWithoutARecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		panes    []rexPane
		recorded string
		want     string
		state    string
	}{
		{"the oldest pane that reports", []rexPane{
			pane("block:3", true, rexRecord{State: "blocked"}),
			pane("block:1", true),
			pane("block:2", true, rexRecord{State: "idle"}),
		}, "", "block:2", "idle"},
		{"the oldest pane when none reports", []rexPane{pane("block:2", true), pane("block:1", true)}, "", "block:1", "open"},
		{"a detached pane isn't it", []rexPane{pane("block:1", false, rexRecord{State: "done"}), pane("block:2", true)}, "", "block:2", "open"},
		{"a recorded pane that closed", []rexPane{pane("block:2", true, rexRecord{State: "done"})}, "block:1", "block:2", "done"},
		{"a worker's record counts with its program's", []rexPane{
			pane("block:1", true, rexRecord{State: "idle"}, rexRecord{ID: "build", State: "blocked", Kind: "question"}),
		}, "block:1", "block:1", "blocked"},
	} {
		s := resolveAgents([]rexSession{{ID: "s", Panes: tc.panes}}, map[string]string{"s": tc.recorded})[0]
		if s.Block != tc.want || s.State != tc.state {
			t.Errorf("%s: got %s on %s; want %s on %s", tc.name, s.State, s.Block, tc.state, tc.want)
		}
	}
	// No pane at all: the session is open and says nothing.
	if s := resolveAgents([]rexSession{{ID: "s", Panes: []rexPane{pane("block:1", false)}}}, nil)[0]; s.State != "open" || s.Block != "" {
		t.Errorf("no placed pane: got %s on %q", s.State, s.Block)
	}
}

func TestDecodeSessions(t *testing.T) {
	// A Lua table with no entries is {}, for the panes and for a pane's records alike.
	out := `[{"id":"s1","label":"fld-1","panes":[
		{"id":"b1","label":"shell","placed":true,"cmd":"/bin/claude","dir":"/w","records":[{"id":"","state":"blocked","kind":"question","msg":"Which one?","at":"2026-11-12T18:16:30.386655Z"}]},
		{"id":"b2","label":"shell","placed":false,"records":{}}]},
		{"id":"s2","label":"empty","panes":{}}]`
	sessions, err := decodeSessions(out)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("decodeSessions = %v, %v", sessions, err)
	}
	p := sessions[0].Panes
	if len(p) != 2 || !p[0].Placed || p[1].Placed || p[0].Proc != (rexProc{"/bin/claude", "/w"}) || len(p[1].Records) != 0 {
		t.Fatalf("panes = %+v", p)
	}
	if r := p[0].Records[0]; r.State != "blocked" || r.Kind != "question" || r.Msg != "Which one?" || r.At.IsZero() {
		t.Errorf("record = %+v", r)
	}
	if len(sessions[1].Panes) != 0 {
		t.Errorf("empty session's panes = %+v", sessions[1].Panes)
	}
	if got := resolveAgents(sessions, nil)[0]; !got.runsClaudeIn("/w") {
		t.Error("the pane's program isn't among the session's")
	}
	if sessions, err := decodeSessions("{}"); err != nil || sessions != nil {
		t.Errorf("no sessions: got %v, %v", sessions, err)
	}
}

func TestPictureApply(t *testing.T) {
	at := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	p := &rexPicture{}
	p.set([]rexSession{{ID: "s1", Label: "fld-1", Panes: []rexPane{pane("b1", true, rexRecord{State: "working", Msg: "one", At: at})}}}, true)
	status := func(state, msg string, when time.Time) rexEvent {
		return rexEvent{Type: "status", Session: "s1", Block: "b1", wireRecord: wireRecord{State: state, Msg: msg, At: when.Format(time.RFC3339Nano)}}
	}
	state := func() string {
		s, _ := p.snapshot()
		if len(s) == 0 {
			return "none"
		}
		return resolveAgents(s, nil)[0].State + " " + resolveAgents(s, nil)[0].Msg
	}
	for _, tc := range []struct {
		name            string
		ev              rexEvent
		changed, resync bool
		want            string
	}{
		{"a new message", status("working", "two", at.Add(time.Second)), true, false, "working two"},
		{"the same again", status("working", "two", at.Add(2*time.Second)), false, false, "working two"},
		{"one from before the listing", status("idle", "", at.Add(-time.Minute)), false, false, "working two"},
		{"blocked", rexEvent{Type: "status", Session: "s1", Block: "b1", wireRecord: wireRecord{State: "blocked", Kind: "permission"}}, true, false, "blocked "},
		{"a worker's record", rexEvent{Type: "status", Session: "s1", Block: "b1", wireRecord: wireRecord{ID: "job", State: "error"}}, true, false, "blocked "},
		{"its program's record removed", rexEvent{Type: "removed", Session: "s1", Block: "b1", IDs: json.RawMessage(`[""]`)}, true, false, "error "},
		{"nothing removed", rexEvent{Type: "removed", Session: "s1", Block: "b1", IDs: json.RawMessage(`{}`)}, false, false, "error "},
		{"a pane it hasn't seen", rexEvent{Type: "status", Session: "s1", Block: "b9", wireRecord: wireRecord{State: "idle"}}, false, true, "error "},
		{"a session it hasn't seen", rexEvent{Type: "status", Session: "s9", Block: "b1", wireRecord: wireRecord{State: "idle"}}, false, true, "error "},
		{"a new session", rexEvent{Type: "sync"}, false, true, "error "},
		{"renamed", rexEvent{Type: "renamed", Session: "s1", Label: "something-else"}, true, false, "error "},
		{"its pane closed", rexEvent{Type: "closed", Session: "s1", Block: "b1"}, true, false, "open "},
		{"closed twice", rexEvent{Type: "closed", Session: "s1", Block: "b1"}, false, false, "open "},
		{"the session gone", rexEvent{Type: "gone", Session: "s1"}, true, false, "none"},
		{"gone twice", rexEvent{Type: "gone", Session: "s1"}, false, false, "none"},
	} {
		changed, resync := p.apply(tc.ev)
		if changed != tc.changed || resync != tc.resync || state() != tc.want {
			t.Errorf("%s: changed %t, resync %t, %q; want %t, %t, %q", tc.name, changed, resync, state(), tc.changed, tc.resync, tc.want)
		}
	}
}

// A snapshot is the caller's own: resolving it or changing it leaves the picture alone.
func TestPictureSnapshotIsACopy(t *testing.T) {
	p := &rexPicture{}
	p.set([]rexSession{{ID: "s1", Panes: []rexPane{pane("b1", true, rexRecord{State: "idle"})}}}, true)
	s, _ := p.snapshot()
	s[0].Panes[0].Records[0].State = "blocked"
	s[0].Panes = nil
	if again, _ := p.snapshot(); len(again[0].Panes) != 1 || again[0].Panes[0].Records[0].State != "idle" {
		t.Errorf("the picture changed with its snapshot: %+v", again)
	}
}

// While Rex's events arrive, the sessions are read from them: a status report costs no listing.
func TestFollowRexAppliesEvents(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s1", Label: "fld-1", Panes: []rexPane{pane("b1", true, rexRecord{State: "working"})}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 8)
	go followRex(ctx, func() { changed <- struct{}{} })
	wait := func(what string) {
		t.Helper()
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatalf("no change reported after %s", what)
		}
	}
	state := func() string {
		t.Helper()
		sessions, err := agentSessions()
		if err != nil || len(sessions) != 1 {
			t.Fatalf("agentSessions = %v, %v", sessions, err)
		}
		return sessions[0].State + "/" + sessions[0].Kind
	}

	f.stream <- rexEvent{Type: "listening"}
	wait("the stream started")
	if state() != "working/" {
		t.Fatalf("after the first listing: %s", state())
	}
	listed := f.listed()

	f.stream <- rexEvent{Type: "status", Session: "s1", Block: "b1", wireRecord: wireRecord{State: "blocked", Kind: "question", Msg: "Which one?"}}
	wait("a status")
	if state() != "blocked/question" {
		t.Errorf("after the status: %s", state())
	}
	if f.listed() != listed {
		t.Errorf("a status report listed the sessions %d more times", f.listed()-listed)
	}

	// A new session takes a listing.
	f.mu.Lock()
	f.open = append(f.open, rexSession{ID: "s2", Label: "fld-2"})
	f.mu.Unlock()
	f.stream <- rexEvent{Type: "sync"}
	wait("a new session")
	if sessions, _ := agentSessions(); len(sessions) != 2 {
		t.Errorf("after a new session: %d sessions", len(sessions))
	}

	// Without the stream the picture can't be trusted: the sessions are listed on each read.
	f.stream <- rexEvent{Type: "down"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, live := following.Load().snapshot(); !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the picture stayed live after the stream ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	listed = f.listed()
	if _, err := agentSessions(); err != nil || f.listed() != listed+1 {
		t.Errorf("with the stream down: %d listings, %v; want one", f.listed()-listed, err)
	}
}

func TestNewSessionRecordsItsPane(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s-open", Label: "fld-1-x"})
	if _, err := updateState(func(s *State) {
		s.Panes["s-open"] = "block:open"
		s.Panes["s-closed"] = "block:closed"
		// Closed, but still a dispatch's session: its pane stays with it.
		s.Sessions["FLD-7"], s.Panes["s-resumable"] = "s-resumable", "block:r"
	}); err != nil {
		t.Fatal(err)
	}
	id, name, err := newSession("fld-1-x", "/w", "claude", []string{"claude"})
	if err != nil || name != "fld-1-x-2" || id != "session:fld-1-x-2" {
		t.Fatalf("newSession = %q, %q, %v; want the label's second", id, name, err)
	}
	panes := loadState().Panes
	if panes[id] != "block:fld-1-x-2" || panes["s-open"] != "block:open" || panes["s-resumable"] != "block:r" {
		t.Errorf("panes = %v; want the new session's and the open one's", panes)
	}
	if _, kept := panes["s-closed"]; kept {
		t.Error("a closed session's pane was kept")
	}
	if !slices.Contains(f.calls, "new fld-1-x-2") {
		t.Errorf("calls = %v", f.calls)
	}
}

// gitCheckout makes a main checkout and a worktree of it under a temp dir, as git lays them out.
func gitCheckout(t *testing.T, repo, worktree string) (main, wt string) {
	t.Helper()
	root := t.TempDir()
	main, wt = filepath.Join(root, repo), filepath.Join(root, worktree)
	gitdir := filepath.Join(main, ".git", "worktrees", worktree)
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return main, wt
}

func TestRepoOf(t *testing.T) {
	main, wt := gitCheckout(t, "reports-app", "reports-app-fr-accents")
	for _, c := range []struct{ key, dir, want string }{
		{"FLD-1780", wt, "reports-app"}, // a worktree named by hand
		{"FLD-1", main, "reports-app"},  // the checkout itself
		{"FLD-1707", "/gone/reports-app-fld-1707", "reports-app"},
		{"reports-app#1185", "", "reports-app"},
		{"fix:reports-app#1185", "", "reports-app"},
		{"task:foreman-3", "", "foreman"},
		{"orchestrator", t.TempDir(), ""}, // not a checkout
	} {
		if got := repoOf(c.key, c.dir); got != c.want {
			t.Errorf("repoOf(%q, %q) = %q, want %q", c.key, c.dir, got, c.want)
		}
	}
}

// A new session goes after the last open one working in the same repo, so the sidebar keeps a
// repo's sessions together.
func TestNewSessionJoinsItsRepo(t *testing.T) {
	_, wt1 := gitCheckout(t, "app", "app-fld-1")
	_, wt3 := gitCheckout(t, "app", "app-fld-3")
	f := useFakeRex(t,
		rexSession{ID: "s1", Label: "fld-1"}, rexSession{ID: "s2", Label: "other"},
		rexSession{ID: "s3", Label: "fld-3"}, rexSession{ID: "s4", Label: "fld-4"})
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"], s.Sessions["FLD-1"] = wt1, "s1"
		s.Dispatched["FLD-3"], s.Sessions["FLD-3"] = wt3, "s3"
		s.Dispatched["FLD-4"], s.Sessions["FLD-4"] = "/gone/elsewhere-fld-4", "s4"
		s.Dispatched["FLD-5"], s.Sessions["FLD-5"] = wt1, "s-closed"
	}); err != nil {
		t.Fatal(err)
	}
	_, wt9 := gitCheckout(t, "app", "app-fld-9")
	id, _, err := newSession("fld-9", wt9, "claude", []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "move " + id + " after s3"; !slices.Contains(f.calls, want) {
		t.Errorf("calls = %v; want %q", f.calls, want)
	}
	// Nothing open in its repo: it stays where Rex put it.
	f.calls = nil
	if _, _, err := newSession("x", t.TempDir(), "claude", []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "move ") }) {
		t.Errorf("calls = %v; want no move", f.calls)
	}
}

// A reply goes to the agent's pane, not to whichever pane of its session reports a status.
func TestReplyGoesToTheAgentsPane(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s1", Label: "fld-1-x", Panes: []rexPane{
		pane("block:1", true, rexRecord{State: "idle"}),
		pane("block:0", true, rexRecord{State: "working"}), // older, and not the agent's
	}})
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"], s.Sessions["FLD-1"], s.Panes["s1"] = "/w", "s1", "block:1"
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runReply([]string{"FLD-1", "carry on"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if want := []string{"send s1 block:1 carry on", "press s1 block:1 enter"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v; want %v", f.calls, want)
	}
}

func TestOpenHome(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s9", Label: "other"})
	id, err := openHome("/bin/foreman")
	if err != nil || id != "session:foreman" || len(f.calls) != 1 {
		t.Fatalf("not running: openHome = %q, %v, calls %v; want it created", id, err, f.calls)
	}
	// One pane running this foreman, which draws the board and the queue itself.
	for _, want := range []string{"create foreman foreman ", `"flavor":"com.superlogical.terminal.shell"`, `"label":"foreman"`, `"command":["/bin/foreman"]`} {
		if !strings.Contains(f.calls[0], want) {
			t.Errorf("the session lacks %s: %s", want, f.calls[0])
		}
	}
	if strings.Contains(f.calls[0], "split") {
		t.Errorf("the session has a split: %s", f.calls[0])
	}
	f = useFakeRex(t, rexSession{ID: "s1", Label: "foreman"})
	if id, err := openHome("/bin/foreman"); err != nil || id != "s1" || len(f.calls) != 0 {
		t.Errorf("running: openHome = %q, %v, calls %v; want its ID and no call", id, err, f.calls)
	}
}

func TestOpenPrintsOrShows(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s1", Label: "foreman"})
	var out strings.Builder
	if err := runOpen([]string{"--print"}, &out); err != nil || out.String() != "s1\n" || len(f.calls) != 0 {
		t.Errorf("open --print: %q, %v, calls %v; want the ID and no call", out.String(), err, f.calls)
	}
	if err := runOpen(nil, &out); err != nil || !slices.Equal(f.calls, []string{"show s1"}) {
		t.Errorf("open: %v, calls %v; want it shown", err, f.calls)
	}
	if err := runOpen([]string{"--nope"}, &out); err == nil {
		t.Error("open --nope: want a usage error")
	}
}

// What a blocked agent waits for is the kind it reports: y and n answer a prompt, nothing else.
func TestBlockedKinds(t *testing.T) {
	item := func(kind, doing string) queueItem {
		return queueItem{Kind: agentItem, Group: needsYou, Key: "FLD-1", Agent: "blocked", Waits: kind, Doing: doing}
	}
	for _, tc := range []struct {
		kind      string
		pressable bool
		record    string
	}{
		{"", true, "kind=permission"},
		{"permission", true, "kind=permission"},
		{"question", false, "kind=question"},
		{"auth", false, "kind=auth"},
	} {
		it := item(tc.kind, "")
		if pressable(it) != tc.pressable {
			t.Errorf("kind %q: pressable = %t", tc.kind, pressable(it))
		}
		if rec := itemRecords([]queueItem{it})["FLD-1"]; !strings.Contains(rec, tc.record) {
			t.Errorf("kind %q: record %q lacks %s", tc.kind, rec, tc.record)
		}
	}
	if pressable(queueItem{Kind: agentItem, Agent: "working"}) {
		t.Error("a working agent has no prompt to press a key on")
	}
	// The record says why in the agent's own words, when it gives them.
	if rec := itemRecords([]queueItem{item("question", "Which branch?")})["FLD-1"]; !strings.Contains(rec, "V2hpY2ggYnJhbmNoPw") {
		t.Errorf("record %q lacks the agent's message", rec)
	}
	// The orchestrator is told what kind of thing it may not answer.
	err := replyAllowed(item("question", ""), false, byOrchestrator)
	if err == nil || !strings.Contains(err.Error(), "asks a question") {
		t.Errorf("orchestrator to a question: %v", err)
	}
	if err := replyAllowed(item("question", ""), false, byMe); err != nil {
		t.Errorf("my reply to a question: %v", err)
	}
}
