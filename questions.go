package main

// Questions from agents. Before an agent stops to ask me something, it runs `foreman ask`, which
// puts the question to the orchestrator and waits for the answer inside the agent's own command:
// nothing is typed into a pane mid-turn. The orchestrator answers what I've already settled and
// escalates the rest, which the queue then shows as needing me.

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Question is one an agent is waiting on in `foreman ask`.
type Question struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
	// Escalated is set when the question waits on me: the orchestrator's suggestion, or why it
	// never reached the orchestrator.
	Escalated string `json:"escalated,omitempty"`
	Answer    string `json:"answer,omitempty"`
	By        string `json:"by,omitempty"` // orchestrator or me
}

const (
	byAgent        = "agent"
	byOrchestrator = "orchestrator"
	byMe           = "me"
	// roleEnv tells foreman, in the orchestrator's commands, who is answering.
	roleEnv = "FOREMAN_ROLE"
	// maxOrchestratorAnswers bounds how much of a dispatch the orchestrator decides for me; past
	// it, the agent's questions come to me.
	maxOrchestratorAnswers = 3
	// askWait stays under the 10 minutes an agent's shell command may run.
	askWait = 8 * time.Minute
)

func role() string {
	if os.Getenv(roleEnv) == byOrchestrator {
		return byOrchestrator
	}
	return byMe
}

// A logEntry is one line of a dispatch's conversation with the orchestrator and me, kept beside its
// report: a new dispatch empties it, a cleanup removes it.
type logEntry struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`   // agent, orchestrator, me
	Kind string    `json:"kind"` // question, answer, escalation, reply, expired
	Text string    `json:"text"`
}

func logFile(key string) string { return filepath.Join(evidenceDir(key), "orchestrator.jsonl") }

func appendLog(key string, e logEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if err := os.MkdirAll(evidenceDir(key), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logFile(key), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return err
}

func readLog(key string) []logEntry {
	f, err := os.Open(logFile(key))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []logEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e logEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// orchestratorSent counts what the orchestrator answered or typed to the dispatch.
func orchestratorSent(key string) int {
	n := 0
	for _, e := range readLog(key) {
		if e.By == byOrchestrator && (e.Kind == "answer" || e.Kind == "reply") {
			n++
		}
	}
	return n
}

// askerKey is the dispatch asking: the one whose recorded session is session, else the one whose
// directory holds cwd.
func askerKey(s State, session, cwd string) (string, error) {
	if session != "" {
		for key, id := range s.Sessions {
			if id == session {
				return key, nil
			}
		}
	}
	cwd = filepath.Clean(cwd)
	for key, dir := range s.Dispatched {
		if cwd == dir || strings.HasPrefix(cwd, dir+string(filepath.Separator)) {
			return key, nil
		}
	}
	return "", errors.New("not a dispatched agent's session or worktree; pass --key")
}

func runAsk(argv []string, stdin io.Reader, out io.Writer) error {
	fs := newFlags("ask")
	key := fs.String("key", "", "")
	wait := fs.Duration("timeout", askWait, "")
	if err := fs.Parse(argv); err != nil {
		return usageError(err)
	}
	text := strings.Join(fs.Args(), " ")
	if text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if text = strings.TrimSpace(text); text == "" {
		return usageError(errors.New("ask takes the question"))
	}
	s := loadState()
	if *key == "" {
		cwd, _ := os.Getwd()
		var err error
		if *key, err = askerKey(s, os.Getenv("REX_SESSION"), cwd); err != nil {
			return err
		}
	}
	answer, err := ask(*key, text, *wait, time.Second, time.Sleep, notifyOrchestrator)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, answer)
	return nil
}

// ask records the question, puts it to the orchestrator, and polls until it is answered, or wait
// runs out. Without an orchestrator to hear it, it waits on me in the queue.
func ask(key, text string, wait, poll time.Duration, sleep func(time.Duration), notify func(key, text string) error) (string, error) {
	q := Question{Text: text, At: time.Now()}
	if err := notify(key, text); err != nil {
		q.Escalated = "The orchestrator didn't get it: " + firstLine(err.Error())
	}
	if _, err := updateState(func(s *State) { s.Questions[key] = q }); err != nil {
		return "", err
	}
	_ = appendLog(key, logEntry{At: q.At, By: byAgent, Kind: "question", Text: text})
	for waited := time.Duration(0); waited < wait; waited += poll {
		sleep(poll)
		got, ok := loadState().Questions[key]
		if !ok || !got.At.Equal(q.At) {
			return "", errors.New("the question was withdrawn: the agent was dispatched again")
		}
		if got.Answer != "" {
			_, _ = updateState(func(s *State) { clearQuestion(s, key, q.At) })
			return got.Answer, nil
		}
	}
	_, _ = updateState(func(s *State) { clearQuestion(s, key, q.At) })
	_ = appendLog(key, logEntry{By: byAgent, Kind: "expired", Text: "no answer in " + wait.String()})
	return "", fmt.Errorf("no answer in %s. Put the question in your report and stop to ask me; my answer will come as a message", wait)
}

func clearQuestion(s *State, key string, at time.Time) {
	if q, ok := s.Questions[key]; ok && q.At.Equal(at) {
		delete(s.Questions, key)
	}
}

// notifyOrchestrator types the question into the orchestrator's pane, as one line. Claude Code
// queues it while the orchestrator works.
func notifyOrchestrator(key, text string) error {
	sessions, err := agentSessions()
	if err != nil {
		return err
	}
	o, ok := findOrchestrator(sessions, loadState().Orchestrator)
	if !ok {
		return errors.New("it isn't running")
	}
	line := strings.Join(strings.Fields(text), " ")
	return sendToAgent(o.ID, o.Block, fmt.Sprintf("[foreman ask] %s asks: %s (it waits for foreman answer %s or foreman escalate %s)", key, line, key, key))
}

// answerQuestion answers the dispatch's pending question. The orchestrator gets
// maxOrchestratorAnswers per dispatch; past them, the question is mine.
func answerQuestion(key, text, by string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("nothing to answer with")
	}
	if by == byOrchestrator && orchestratorSent(key) >= maxOrchestratorAnswers {
		return fmt.Errorf("you've answered %s %d times already; escalate it to me", key, maxOrchestratorAnswers)
	}
	found := false
	if _, err := updateState(func(s *State) {
		q, ok := s.Questions[key]
		if !ok || q.Answer != "" {
			return
		}
		found = true
		q.Answer, q.By = text, by
		s.Questions[key] = q
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s has no question waiting", key)
	}
	return appendLog(key, logEntry{By: by, Kind: "answer", Text: text})
}

// escalateQuestion hands the dispatch's pending question to me, with the orchestrator's suggestion.
func escalateQuestion(key, note string) error {
	note = cmp.Or(strings.TrimSpace(note), "no suggestion")
	found := false
	if _, err := updateState(func(s *State) {
		q, ok := s.Questions[key]
		if !ok || q.Answer != "" {
			return
		}
		found = true
		q.Escalated = note
		s.Questions[key] = q
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s has no question waiting", key)
	}
	return appendLog(key, logEntry{By: role(), Kind: "escalation", Text: note})
}

// resolveKey is the dispatched key, in any case.
func resolveKey(s State, key string) (string, error) {
	for k := range s.Dispatched {
		if strings.EqualFold(k, key) {
			return k, nil
		}
	}
	return "", fmt.Errorf("no dispatched agent %q; foreman agents lists them", key)
}

func runAnswer(argv []string, stdin io.Reader, out io.Writer, escalate bool) error {
	name := "answer"
	if escalate {
		name = "escalate"
	}
	fs := newFlags(name)
	if err := fs.Parse(argv); err != nil {
		return usageError(err)
	}
	if fs.NArg() < 1 || (!escalate && fs.NArg() < 2) {
		return usageError(fmt.Errorf("%s takes KEY and TEXT", name))
	}
	text := strings.Join(fs.Args()[1:], " ")
	if text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	key, err := resolveKey(loadState(), fs.Arg(0))
	if err != nil {
		return err
	}
	if escalate {
		if err := escalateQuestion(key, text); err != nil {
			return err
		}
		fmt.Fprintln(out, "✓ "+key+"'s question waits on you in the queue")
		return nil
	}
	if err := answerQuestion(key, text, role()); err != nil {
		return err
	}
	fmt.Fprintln(out, "✓ Answered "+key)
	return nil
}

// logLines renders the log for the queue's detail pane.
func logLines(entries []logEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	verbs := map[string]string{"question": "asks", "answer": "answers", "escalation": "hands it to you", "reply": "sends", "expired": "gave up"}
	out := []string{"## Questions", ""}
	for _, e := range entries {
		line := fmt.Sprintf("- %s **%s %s**", e.At.Local().Format("15:04"), e.By, verbs[e.Kind])
		if e.Text != "" {
			line += ": " + strings.Join(strings.Fields(e.Text), " ")
		}
		out = append(out, line)
	}
	return out
}
