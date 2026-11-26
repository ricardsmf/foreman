package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// toldIn keeps the notifications a test raises, in place of showing them.
func toldIn(t *testing.T) *[]string {
	t.Helper()
	told, old := &[]string{}, notify
	notify = func(_, text, _ string) error { *told = append(*told, text); return nil }
	t.Cleanup(func() { notify = old })
	return told
}

// askersRunning names the foreman asks that still run: every other pid is gone.
func askersRunning(t *testing.T, pids ...int) {
	t.Helper()
	old := askRuns
	askRuns = func(pid int) bool { return slices.Contains(pids, pid) }
	t.Cleanup(func() { askRuns = old })
}

func TestNewQuestion(t *testing.T) {
	q, err := newQuestion(" Which store? ", []string{" Redis ", "", "Postgres"}, "postgres", true)
	if err != nil || q.Text != "Which store?" || !slices.Equal(q.Options, []string{"Redis", "Postgres"}) || q.Recommended != "Postgres" {
		t.Errorf("newQuestion = %+v, %v", q, err)
	}
	if q, err := newQuestion("Go on?", nil, "Yes, behind a flag", true); err != nil || q.Recommended != "Yes, behind a flag" {
		t.Errorf("a recommendation without options: %+v, %v", q, err)
	}
	for _, tc := range []struct {
		name      string
		options   []string
		recommend string
		proceed   bool
		want      string
	}{
		{"recommends none of the options", []string{"A", "B"}, "C", false, "none of the options"},
		{"goes on with nothing", []string{"A", "B"}, "", true, "--proceed needs --recommend"},
		{"ten options", strings.Split("a b c d e f g h i j", " "), "", false, "at most 9"},
	} {
		if _, err := newQuestion("Which?", tc.options, tc.recommend, tc.proceed); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// A question raises a notification as it is asked: its agent shows as working, so nothing else
// says outside the queue that it waits on me.
func TestAskNotifies(t *testing.T) {
	tempState(t)
	oldHome := home
	home = t.TempDir()
	t.Cleanup(func() { home = oldHome })
	dispatched(t, "FLD-1", "/w/app-fld-1")
	told := toldIn(t)
	var out strings.Builder
	err := runAsk([]string{"--key", "FLD-1", "--timeout", "0s", "Which\nendpoint?"}, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "stop to ask me") {
		t.Fatalf("unanswered: %v", err)
	}
	if want := []string{"FLD-1 asks: Which endpoint?"}; !slices.Equal(*told, want) {
		t.Errorf("told %q, want %q", *told, want)
	}
}

// A click on a question's notification shows the session of the agent asking.
func TestAskShowsTheAgentsSession(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	if _, err := updateState(func(s *State) { d := s.Dispatched["FLD-1"]; d.Session = "s1"; s.Dispatched["FLD-1"] = d }); err != nil {
		t.Fatal(err)
	}
	toldIn(t)
	var shown []string
	notify = func(_, _, session string) error { shown = append(shown, session); return nil }
	var out strings.Builder
	if err := runAsk([]string{"--key", "FLD-1", "--recommend", "yes", "--proceed", "Flag it?"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(shown, []string{"s1"}) {
		t.Errorf("the notification shows %q, want s1", shown)
	}
}

// With --proceed the agent goes on with what it recommends: the command returns at once, and the
// question waits in the queue for me to confirm.
func TestAskProceeds(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	told := toldIn(t)
	var out strings.Builder
	// Options after the question, as an agent writes them.
	argv := []string{"--key", "FLD-1", "Which store?", "--option", "Redis", "--option", "Postgres", "--recommend", "Postgres", "--proceed"}
	if err := runAsk(argv, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Go on with “Postgres”") {
		t.Errorf("out = %q", out.String())
	}
	s := loadState()
	if _, waits := s.Questions["FLD-1"]; waits {
		t.Error("it went on, yet a question waits")
	}
	ds := s.Decisions["FLD-1"]
	if len(ds) != 1 || ds[0].Text != "Which store?" || ds[0].Recommended != "Postgres" || len(ds[0].Options) != 2 || ds[0].Pid != 0 {
		t.Fatalf("decisions = %+v", ds)
	}
	if want := []string{"FLD-1 went on with “Postgres”: Which store?"}; !slices.Equal(*told, want) {
		t.Errorf("told %q, want %q", *told, want)
	}
	if log := readLog("FLD-1"); len(log) != 1 || log[0].Kind != "decision" || log[0].Text != "Which store? [Redis | Postgres] (recommends: Postgres)" {
		t.Errorf("log = %+v", log)
	}
	if err := runAsk([]string{"--key", "FLD-1", "--proceed", "Which?"}, strings.NewReader(""), &out); err == nil {
		t.Error("went on with no recommendation")
	}
	// A second one waits behind the first.
	if err := runAsk([]string{"--key", "FLD-1", "--recommend", "yes", "--proceed", "Flag it?"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if ds := loadState().Decisions["FLD-1"]; len(ds) != 2 || ds[1].Text != "Flag it?" {
		t.Errorf("decisions = %+v", ds)
	}
}

// A question outlives what asked it: with its session closed, or its foreman ask gone, it stays
// under needs you as one the agent left.
func TestQueueKeepsALeftQuestion(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	askersRunning(t, 7)
	ask := func(pid int) {
		t.Helper()
		if _, err := updateState(func(s *State) {
			s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" })
			s.Questions["FLD-1"] = Question{Text: "Which endpoint?", At: time.Unix(1700000000, 0), Pid: pid}
		}); err != nil {
			t.Fatal(err)
		}
	}
	open := []rexSession{{ID: "s1", Label: "fld-1", State: "idle"}}
	for _, tc := range []struct {
		name     string
		pid      int
		sessions []rexSession
		left     bool
		label    string
	}{
		{"it waits in foreman ask", 7, open, false, "asks you"},
		{"from before the pid was kept", 0, open, false, "asks you"},
		{"its foreman ask is gone", 8, open, true, "left a question"},
		{"its session closed", 7, nil, true, "left a question"},
	} {
		ask(tc.pid)
		it := queueItems(loadState(), tc.sessions, nil)[0]
		_, label := itemLabel(it)
		if it.Left != tc.left || it.Group != needsYou || !strings.Contains(label, tc.label) || asking(it) == tc.left {
			t.Errorf("%s: left %t, group %d, label %q, asking %t", tc.name, it.Left, it.Group, label, asking(it))
		}
	}
	// Marked reviewed while it waited, it comes back once nothing waits on it.
	ask(7)
	it := queueItems(loadState(), open, nil)[0]
	_, _ = updateState(func(s *State) { s.Dismissed["FLD-1"] = it.Sig })
	if it = queueItems(loadState(), open, nil)[0]; it.Group == needsYou {
		t.Fatalf("marked reviewed: group %d", it.Group)
	}
	if it = queueItems(loadState(), nil, nil)[0]; it.Group != needsYou {
		t.Errorf("left since I marked it: group %d", it.Group)
	}
	detail := strings.Join(itemDetail(it), "\n")
	for _, want := range []string{"# Left a question", "An answer resumes it", "Which endpoint?", "`f` answers it."} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
}

// The answer to a question a closed session left resumes the agent with it, as the turn it starts
// on; the orchestrator resumes nothing.
func TestAnswerResumesAClosedAgent(t *testing.T) {
	f := useFakeRex(t)
	oldEvidence := evidenceRoot
	evidenceRoot = t.TempDir()
	t.Cleanup(func() { evidenceRoot = oldEvidence })
	dir := t.TempDir()
	at := time.Unix(1700000000, 0)
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"] = Dispatched{Dir: dir, Session: "gone", Agent: agentPi, Title: "My van"}
		s.Questions["FLD-1"] = Question{Text: "Which\nendpoint?", At: at, Pid: 7}
	}); err != nil {
		t.Fatal(err)
	}
	s := loadState()
	it := queueItems(s, nil, nil)[0]
	if _, err := answerItem(s, it, "v2", byOrchestrator); err == nil || !strings.Contains(err.Error(), "only I resume") || len(f.calls) != 0 {
		t.Fatalf("the orchestrator's answer: %v, calls %q", err, f.calls)
	}
	said, err := answerItem(s, it, " v2 ", byMe)
	if err != nil || said != "✓ Resumed FLD-1 with the answer" {
		t.Fatalf("answerItem = %q, %v", said, err)
	}
	label := sessionLabel("FLD-1", "My van")
	cmd := f.cmds[label]
	if len(cmd) != 3 || cmd[1] != "--continue" || cmd[2] != "You asked me “Which endpoint?”. My answer: v2" {
		t.Errorf("resumed with %q", cmd)
	}
	s = loadState()
	if _, waits := s.Questions["FLD-1"]; waits || s.Dispatched["FLD-1"].Session != "session:"+label {
		t.Errorf("after the answer: questions %+v, session %q", s.Questions, s.Dispatched["FLD-1"].Session)
	}
	if log := readLog("FLD-1"); len(log) != 1 || log[0].By != byMe || log[0].Kind != "answer" || log[0].Text != "v2" {
		t.Errorf("log = %+v", log)
	}
	// Claude Code takes the message after `--`: --add-dir would take it for a directory.
	if cmd := resumeCommand(agentClaude, "go on"); !slices.Equal(cmd[len(cmd)-3:], []string{evidenceRoot, "--", "go on"}) || cmd[1] != "--continue" {
		t.Errorf("claude resumes with %q", cmd)
	}
}

// The answer to a question whose foreman ask is gone, its session still open, is typed into the
// agent's pane, saying which question it answers.
func TestAnswerToAGoneAskIsTyped(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s1", Label: "fld-1-x", Panes: []rexPane{pane("block:1", true, rexRecord{State: "idle"})}})
	oldEvidence := evidenceRoot
	evidenceRoot = t.TempDir()
	t.Cleanup(func() { evidenceRoot = oldEvidence })
	askersRunning(t)
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"], s.Panes["s1"] = Dispatched{Dir: "/w", Session: "s1"}, "block:1"
		s.Questions["FLD-1"] = Question{Text: "Which endpoint?", At: time.Unix(1700000000, 0), Pid: 7}
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runAnswer([]string{"fld-1", "v2"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if want := []string{"send s1 block:1 You asked me “Which endpoint?”. My answer: v2", "press s1 block:1 enter"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if _, waits := loadState().Questions["FLD-1"]; waits || !strings.Contains(out.String(), "in its pane") {
		t.Errorf("after the answer: still waiting %t, said %q", waits, out.String())
	}
}

// What an agent went on with waits under review. Its own answer confirms it and sends nothing;
// another one reaches it as a message; marking the row reviewed confirms the rest.
func TestDecisionsWaitToBeConfirmed(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s1", Label: "fld-1-x", Panes: []rexPane{pane("block:1", true, rexRecord{State: "working", Msg: "Running the tests"})}})
	oldEvidence := evidenceRoot
	evidenceRoot = t.TempDir()
	t.Cleanup(func() { evidenceRoot = oldEvidence })
	store := Question{Text: "Which store?", At: time.Unix(1700000000, 0), Options: []string{"Redis", "Postgres"}, Recommended: "Postgres"}
	flag := Question{Text: "Flag it?", At: time.Unix(1700000060, 0), Recommended: "Yes"}
	name := Question{Text: "Name?", At: time.Unix(1700000120, 0), Recommended: "van"}
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"], s.Panes["s1"] = Dispatched{Dir: "/w", Session: "s1"}, "block:1"
		s.Decisions["FLD-1"] = []Question{store, flag, name}
	}); err != nil {
		t.Fatal(err)
	}
	item := func() queueItem {
		t.Helper()
		sessions, err := agentSessions()
		if err != nil {
			t.Fatal(err)
		}
		return queueItems(loadState(), sessions, nil)[0]
	}
	it := item()
	if _, label := itemLabel(it); it.Group != toReview || !strings.Contains(label, "went on, 3 to confirm") {
		t.Fatalf("group %d, label %q", it.Group, label)
	}
	if rec := itemRecords([]queueItem{it})["FLD-1"]; !strings.Contains(rec, "state=done") {
		t.Errorf("its record for Rex: %q", rec)
	}
	detail := strings.Join(itemDetail(it), "\n")
	for _, want := range []string{"# Went on without an answer", "1. Redis", "2. Postgres **(went on with this)**", "`a` confirms it, `1`–`2` or `f` send another answer", "It went on with: **Yes**"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	// Another answer is typed into its pane, though it works: it went on, and should hear of it.
	if said, err := answerItem(loadState(), it, "Redis", byMe); err != nil || !strings.Contains(said, "in its pane") {
		t.Fatalf("another answer: %q, %v", said, err)
	}
	if want := "send s1 block:1 You asked me “Which store?” and went on with “Postgres”. My answer is different: Redis"; !slices.Contains(f.calls, want) {
		t.Errorf("calls = %q", f.calls)
	}
	f.calls = nil
	// Its own answer confirms the next, and nothing is sent.
	it = item()
	if said, err := answerItem(loadState(), it, "yes", byMe); err != nil || !strings.Contains(said, "Confirmed") || len(f.calls) != 0 {
		t.Fatalf("its own answer: %q, %v, calls %q", said, err, f.calls)
	}
	// x confirms what is left, and the row goes back to where its agent is.
	q := &queueModel{state: loadState(), details: map[string][]string{}}
	it = item()
	q.items, q.all = []queueItem{it}, []queueItem{it}
	q.input(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if ds := loadState().Decisions["FLD-1"]; len(ds) != 0 || len(f.calls) != 0 {
		t.Errorf("after x: decisions %+v, calls %q", ds, f.calls)
	}
	if it = item(); it.Group != working {
		t.Errorf("after x: group %d", it.Group)
	}
	var kinds []string
	for _, e := range readLog("FLD-1") {
		kinds = append(kinds, e.Kind+" "+e.Text)
	}
	if want := []string{"answer Redis", "confirm Yes", "confirm van"}; !slices.Equal(kinds, want) {
		t.Errorf("log = %q, want %q", kinds, want)
	}
}

// A digit answers with that choice and a with the recommended one, where the question has them;
// f on a question a closed session left takes an answer.
func TestQueueAnswerKeys(t *testing.T) {
	tempState(t)
	asked := Question{Text: "Which store?", At: time.Unix(1700000000, 0), Options: []string{"Redis", "Postgres"}}
	it := queueItem{Kind: agentItem, Key: "FLD-1", Agent: "working", Question: &asked}
	q := &queueModel{items: []queueItem{it}, details: map[string][]string{}}
	press := func(k rune) tea.Cmd { return q.input(tea.KeyPressMsg{Code: k, Text: string(k)}) }
	if cmd := press('3'); cmd != nil || !strings.Contains(q.status, "has 2 choices") {
		t.Errorf("3 of 2 choices: cmd %t, status %q", cmd != nil, q.status)
	}
	if cmd := press('a'); cmd != nil || !strings.Contains(q.status, "recommends nothing") {
		t.Errorf("a with no recommendation: cmd %t, status %q", cmd != nil, q.status)
	}
	if cmd := press('2'); cmd == nil {
		t.Errorf("2 of 2 choices did nothing: %q", q.status)
	}
	q.items = []queueItem{{Kind: agentItem, Key: "FLD-2", Agent: "idle"}}
	if cmd := press('1'); cmd != nil || !strings.Contains(q.status, "asks nothing") {
		t.Errorf("a digit on an agent that asks nothing: cmd %t, status %q", cmd != nil, q.status)
	}
	if hint := answerHint(asked, false); hint != "`1`–`2` answer with a choice, `f` in your own words." {
		t.Errorf("hint = %q", hint)
	}

	// Closed, f refuses an agent that asks nothing and takes an answer for one that left a question.
	q.items = []queueItem{{Kind: agentItem, Key: "FLD-2"}}
	if press('f'); q.feedback != nil {
		t.Fatalf("f on a closed agent: %+v", q.feedback)
	}
	left := queueItem{Kind: agentItem, Key: "FLD-1", Question: &asked, Left: true}
	q.items, q.all = []queueItem{left}, []queueItem{left}
	if press('f'); q.feedback == nil || !q.feedback.answer || !q.feedback.asked.Equal(asked.At) {
		t.Fatalf("f on a question it left: %+v", q.feedback)
	}
	// The question changed while I typed: the answer isn't for the new one.
	q.feedback.text = "Redis"
	later := asked
	later.At = asked.At.Add(time.Minute)
	q.all[0].Question = &later
	if cmd := q.feedbackInput(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil || !strings.Contains(q.status, "no longer asks") {
		t.Errorf("an answer to a question that changed: cmd %t, status %q", cmd != nil, q.status)
	}
}

// A question foreman ask gave up on stays under needs you. Its answer is typed into the agent's
// pane; x sets it aside instead.
func TestExpiredQuestionStaysToAnswer(t *testing.T) {
	f := useFakeRex(t, rexSession{ID: "s1", Label: "fld-1-x", Panes: []rexPane{pane("block:1", true, rexRecord{State: "idle"})}})
	oldEvidence := evidenceRoot
	evidenceRoot = t.TempDir()
	t.Cleanup(func() { evidenceRoot = oldEvidence })
	if _, err := updateState(func(s *State) {
		s.Dispatched["FLD-1"], s.Panes["s1"] = Dispatched{Dir: "/w", Session: "s1"}, "block:1"
	}); err != nil {
		t.Fatal(err)
	}
	expire := func() queueItem {
		t.Helper()
		asked := Question{Text: "Which store?", Options: []string{"Redis", "Postgres"}}
		if _, err := ask("FLD-1", asked, time.Second, time.Second, func(time.Duration) {}, nobodyTyped); err == nil {
			t.Fatal("an unanswered ask returned an answer")
		}
		sessions, err := agentSessions()
		if err != nil {
			t.Fatal(err)
		}
		return queueItems(loadState(), sessions, nil)[0]
	}
	it := expire()
	// This test's own process is the foreman ask, and still runs: gave up is what marks it.
	if _, label := itemLabel(it); !it.Left || it.Group != needsYou || !strings.Contains(label, "left a question") {
		t.Fatalf("expired: left %t, group %d, label %q", it.Left, it.Group, label)
	}
	if detail := strings.Join(itemDetail(it), "\n"); !strings.Contains(detail, "2. Postgres") || !strings.Contains(detail, "`x` sets it aside") {
		t.Errorf("its detail:\n%s", detail)
	}
	if said, err := answerItem(loadState(), it, "Postgres", byMe); err != nil || !strings.Contains(said, "in its pane") {
		t.Fatalf("answerItem = %q, %v", said, err)
	}
	if want := "send s1 block:1 You asked me “Which store?”. My answer: Postgres"; !slices.Contains(f.calls, want) {
		t.Errorf("calls = %q", f.calls)
	}
	if _, waits := loadState().Questions["FLD-1"]; waits {
		t.Error("the answered question still waits")
	}

	it = expire()
	f.calls = nil
	q := &queueModel{state: loadState(), details: map[string][]string{}, items: []queueItem{it}, all: []queueItem{it}}
	q.input(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if _, waits := loadState().Questions["FLD-1"]; waits || len(f.calls) != 0 || !strings.Contains(q.status, "aside") {
		t.Errorf("x on it: still waits %t, calls %q, status %q", waits, f.calls, q.status)
	}
	if log := readLog("FLD-1"); log[len(log)-1].Kind != "aside" {
		t.Errorf("log = %+v", log)
	}
}

// An answer that lands after foreman ask's last look, before it gives up, is still the answer.
func TestAskTakesALateAnswer(t *testing.T) {
	tempState(t)
	dispatched(t, "FLD-1", "/w/app-fld-1")
	late := func() (string, bool) {
		if err := answerQuestion("FLD-1", "v2", byMe); err != nil {
			t.Error(err)
		}
		return "", false
	}
	answer, err := ask("FLD-1", Question{Text: "Which endpoint?"}, time.Second, time.Second, func(time.Duration) {}, late)
	if err != nil || answer != "v2" {
		t.Fatalf("ask = %q, %v", answer, err)
	}
	if _, waits := loadState().Questions["FLD-1"]; waits {
		t.Error("the answered question still waits")
	}
}

// The orchestrator's guide names the two states only its agents listing shows.
func TestOrchestratorGuideNamesQuestionStates(t *testing.T) {
	for _, want := range []string{"`decisions`", "`left`", "`recommended`"} {
		if !strings.Contains(orchestratorGuide, want) {
			t.Errorf("the guide lacks %s", want)
		}
	}
}

// An agent's prompt says how to give choices and how to go on without me.
func TestAskSectionTeachesOptions(t *testing.T) {
	section := strings.Join(askSection(), "\n")
	for _, want := range []string{"`--option", "`--recommend", "`--proceed`"} {
		if !strings.Contains(section, want) {
			t.Errorf("the prompt lacks %s:\n%s", want, section)
		}
	}
}
