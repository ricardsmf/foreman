package main

// Questions from agents. Before an agent stops to ask me something, it runs `foreman ask`, which
// puts the question in the queue, under what needs me, and waits for the answer inside the agent's
// own command: nothing is typed into a pane mid-turn.

import (
	"bufio"
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
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
	Answer string    `json:"answer,omitempty"`
	By     string    `json:"by,omitempty"` // me, or the orchestrator passing my answer on
}

const (
	byAgent        = "agent"
	byOrchestrator = "orchestrator"
	byMe           = "me"
	// roleEnv tells foreman, in the orchestrator's commands, who is answering.
	roleEnv = "FOREMAN_ROLE"
	// askWait stays under the 10 minutes an agent's shell command may run.
	askWait = 8 * time.Minute
)

func role() string {
	if os.Getenv(roleEnv) == byOrchestrator {
		return byOrchestrator
	}
	return byMe
}

// A logEntry is one line of a dispatch's questions and what the orchestrator sent it, kept beside
// its report: a new dispatch empties it, a cleanup removes it.
type logEntry struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`   // agent, orchestrator, me
	Kind string    `json:"kind"` // question, answer, reply, expired
	Text string    `json:"text"`
}

func logFile(key string) string { return filepath.Join(evidenceDir(key), "questions.jsonl") }

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
	answer, err := ask(*key, text, *wait, time.Second, time.Sleep)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, answer)
	return nil
}

// ask records the question, which the queue shows as needing me, and polls until it is answered,
// or wait runs out.
func ask(key, text string, wait, poll time.Duration, sleep func(time.Duration)) (string, error) {
	q := Question{Text: text, At: time.Now()}
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

// answerQuestion answers the dispatch's pending question.
func answerQuestion(key, text, by string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("nothing to answer with")
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

// resolveKey is the dispatched key, in any case.
func resolveKey(s State, key string) (string, error) {
	for k := range s.Dispatched {
		if strings.EqualFold(k, key) {
			return k, nil
		}
	}
	return "", fmt.Errorf("no dispatched agent %q; foreman agents lists them", key)
}

func runAnswer(argv []string, stdin io.Reader, out io.Writer) error {
	fs := newFlags("answer")
	if err := fs.Parse(argv); err != nil {
		return usageError(err)
	}
	if fs.NArg() < 2 {
		return usageError(errors.New("answer takes KEY and TEXT"))
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
	verbs := map[string]string{"question": "asks", "answer": "answers", "reply": "sends", "expired": "gave up"}
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
