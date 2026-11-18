package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A pane's ports come with the listing, and a session's are every pane's, one entry a port number.
func TestSessionPorts(t *testing.T) {
	// A Lua table with no entries is {}, for a pane's ports too.
	out := `[{"id":"s1","label":"fld-1","panes":[
		{"id":"b1","label":"shell","placed":true,"records":{},"ports":[
			{"port":5173,"address":"::1","process":"vite"},{"port":5173,"address":"127.0.0.1","process":"vite"}]},
		{"id":"b2","label":"shell","placed":true,"records":{},"ports":[
			{"port":3000,"address":"192.168.1.20","process":"node"},{"port":8080,"address":"0.0.0.0","process":""},
			{"port":9000,"address":"fd00::1","process":"python3"}]},
		{"id":"b3","label":"shell","placed":true,"records":{},"ports":{}},
		{"id":"b4","label":"shell","placed":true,"records":{}}]}]`
	sessions, err := decodeSessions(out)
	if err != nil || len(sessions) != 1 || len(sessions[0].Panes) != 4 {
		t.Fatalf("decodeSessions = %+v, %v", sessions, err)
	}
	if p := sessions[0].Panes; len(p[0].Ports) != 2 || len(p[2].Ports) != 0 || len(p[3].Ports) != 0 {
		t.Fatalf("panes' ports = %+v", p)
	}
	var urls []string
	for _, p := range resolveAgents(sessions, nil)[0].Ports {
		urls = append(urls, p.url())
	}
	want := []string{"http://192.168.1.20:3000", "http://localhost:5173", "http://localhost:8080", "http://[fd00::1]:9000"}
	if !slices.Equal(urls, want) {
		t.Errorf("urls = %v; want %v", urls, want)
	}
	// The socket localhost reaches stands for the port, whichever pane lists it first.
	got := sessionPorts([]rexPane{
		{Ports: []rexPort{{Port: 3000, Addr: "192.168.1.20"}}},
		{Ports: []rexPort{{Port: 3000, Addr: "127.0.0.1"}}},
	})
	if len(got) != 1 || got[0].url() != "http://localhost:3000" {
		t.Errorf("one port on two addresses = %+v", got)
	}
}

// A pane's ports follow Rex's events, with no listing.
func TestPictureAppliesPorts(t *testing.T) {
	p := &rexPicture{}
	p.set([]rexSession{{ID: "s1", Panes: []rexPane{pane("b1", true)}}}, true)
	const vite = `[{"port":5173,"address":"127.0.0.1","process":"vite"}]`
	for _, tc := range []struct {
		name, block, ports string
		changed            bool
		want               int
	}{
		{"a server starts", "b1", vite, true, 1},
		{"the same list again", "b1", vite, false, 1},
		{"a pane it hasn't seen", "b9", vite, false, 1},
		{"the server stops", "b1", "{}", true, 0},
	} {
		changed, resync := p.apply(rexEvent{Type: "ports", Session: "s1", Block: tc.block, Ports: json.RawMessage(tc.ports)})
		s, _ := p.snapshot()
		if got := len(resolveAgents(s, nil)[0].Ports); changed != tc.changed || resync || got != tc.want {
			t.Errorf("%s: changed %t, resync %t, %d ports; want %t, false, %d", tc.name, changed, resync, got, tc.changed, tc.want)
		}
	}
}

// An agent's row and detail say what its session listens on, and p opens it, one port a press.
func TestQueueOpensPorts(t *testing.T) {
	tempState(t)
	t.Setenv("REX_SESSION", "")
	var opened []string
	oldOpen := openURL
	openURL = func(u string) { opened = append(opened, u) }
	t.Cleanup(func() { openURL = oldOpen })
	q := &queueModel{details: map[string][]string{}, width: 100, height: 30,
		state: State{Dispatched: map[string]Dispatched{"FLD-1": {Dir: "/w/1", Session: "s1"}, "FLD-2": {Dir: "/w/1", Session: "s2"}}},
		sessions: []rexSession{
			{ID: "s1", Label: "fld-1", State: "idle", Ports: []rexPort{{Port: 3000, Addr: "127.0.0.1", Proc: "node"}, {Port: 5173, Addr: "::1", Proc: "vite"}}},
			{ID: "s2", Label: "fld-2", State: "idle"},
		}}
	q.rebuild()
	if it, ok := q.current(); !ok || it.Key != "FLD-1" {
		t.Fatalf("the first row = %+v", it)
	}
	rows := strings.Join(q.render(98, 29), "\n")
	for _, want := range []string{":3000 +1", "http://localhost:3000 (node) · http://localhost:5173 (vite)", "open :3000"} {
		if !strings.Contains(rows, want) {
			t.Errorf("the queue lacks %q:\n%s", want, rows)
		}
	}
	p := tea.KeyPressMsg{Code: 'p', Text: "p"}
	q.input(p)
	if !strings.Contains(q.status, "p again opens :5173") {
		t.Errorf("status after the first p: %q", q.status)
	}
	q.input(p)
	q.input(p)
	if want := []string{"http://localhost:3000", "http://localhost:5173", "http://localhost:3000"}; !slices.Equal(opened, want) {
		t.Errorf("opened %v; want %v", opened, want)
	}

	q.input(tea.KeyPressMsg{Code: tea.KeyDown})
	q.input(p)
	if it, _ := q.current(); it.Key != "FLD-2" || len(opened) != 3 || !strings.Contains(q.status, "listens on a port") {
		t.Errorf("an agent with no port: on %s, opened %v, status %q", it.Key, opened, q.status)
	}
	if rows := strings.Join(q.render(98, 29), "\n"); strings.Contains(rows, "Listening") || strings.Contains(rows, "p open") {
		t.Errorf("its detail names a port:\n%s", rows)
	}
}

// A prompt stays at the top of a blocked agent's detail, over its ports.
func TestQueuePortsUnderAPrompt(t *testing.T) {
	tempState(t)
	t.Setenv("REX_SESSION", "")
	q := &queueModel{details: map[string][]string{}, width: 100, height: 30,
		state:    State{Dispatched: map[string]Dispatched{"FLD-1": {Dir: "/w/1", Session: "s1"}}},
		sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "blocked", Ports: []rexPort{{Port: 5173, Addr: "::1"}}}}}
	q.rebuild()
	it, _ := q.current()
	q.details[detailKey(it)] = []string{"Do you want to proceed?"}
	rows := strings.Join(q.render(98, 29), "\n")
	asks, listens := strings.Index(rows, "Do you want to proceed?"), strings.Index(rows, "http://localhost:5173")
	if asks < 0 || listens < asks {
		t.Errorf("prompt at %d, ports at %d:\n%s", asks, listens, rows)
	}
}

func TestAgentsListPorts(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-2", "/w/app-fld-2")
	_, _ = updateState(func(s *State) { s.change("FLD-2", func(d *Dispatched) { d.Session = "s2" }) })
	s := loadState()
	items := queueItems(s, []rexSession{{ID: "s2", Label: "fld-2", State: "idle", Ports: []rexPort{{Port: 5173, Addr: "::1", Proc: "vite"}}}}, nil)
	it, err := findItem(items, "FLD-2")
	if err != nil {
		t.Fatal(err)
	}
	a := agentInfoOf(it, s)
	if b, _ := json.Marshal(a.Ports); string(b) != `[{"port":5173,"process":"vite","url":"http://localhost:5173"}]` {
		t.Errorf("ports = %s", b)
	}
	var out strings.Builder
	if err := writeAgents(&out, []agentInfo{a}); err != nil || !strings.Contains(out.String(), "report  http://localhost:5173") {
		t.Errorf("agents output: %q, %v", out.String(), err)
	}
}
