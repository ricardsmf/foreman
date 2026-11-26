package main

// Questions from agents. Before an agent stops to ask me something, it runs `foreman ask`, which
// puts the question in the queue, under what needs me, and waits for the answer inside the agent's
// own command: nothing is typed into a pane mid-turn.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// A Question is one an agent put to me with `foreman ask`: one it waits on, or one it went on
// without, taking the answer it recommended.
type Question struct {
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
	Answer string    `json:"answer,omitempty"`
	By     string    `json:"by,omitempty"` // me, or the orchestrator passing my answer on
	// Options are the choices the agent gave, and Recommended the answer it would take: one of
	// them, when it gave any.
	Options     []string `json:"options,omitempty"`
	Recommended string   `json:"recommended,omitempty"`
	// Pid is the foreman ask that waits on the answer. 0 for a question from before it was kept,
	// and for one the agent went on without.
	Pid int `json:"pid,omitempty"`
	// GaveUp is whether that foreman ask stopped waiting: the question is still mine to answer,
	// and the answer goes to the agent as a message.
	GaveUp bool `json:"gaveUp,omitempty"`
}

// maxOptions is how many choices a question takes: the queue answers with a digit.
const maxOptions = 9

// A stringList is a flag given once per value.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ", ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// newQuestion checks what an agent asks: its choices, the one it recommends among them, and that
// it recommends one when it goes on without me.
func newQuestion(text string, options []string, recommend string, proceed bool) (Question, error) {
	q := Question{Text: strings.TrimSpace(text), Recommended: strings.TrimSpace(recommend)}
	if q.Text == "" {
		return q, errors.New("ask takes the question")
	}
	for _, o := range options {
		if o = strings.TrimSpace(o); o != "" {
			q.Options = append(q.Options, o)
		}
	}
	if len(q.Options) > maxOptions {
		return q, fmt.Errorf("ask takes at most %d options, got %d: I answer with a digit", maxOptions, len(q.Options))
	}
	if q.Recommended != "" && len(q.Options) > 0 {
		n := slices.IndexFunc(q.Options, func(o string) bool { return strings.EqualFold(o, q.Recommended) })
		if n < 0 {
			return q, fmt.Errorf("--recommend %q is none of the options: %s", q.Recommended, strings.Join(q.Options, " | "))
		}
		q.Recommended = q.Options[n]
	}
	if proceed && q.Recommended == "" {
		return q, errors.New("--proceed needs --recommend: the answer you go on with")
	}
	return q, nil
}

// askedText is the question as the log keeps it: its text, its choices and the recommended one.
func askedText(q Question) string {
	text := q.Text
	if len(q.Options) > 0 {
		text += " [" + strings.Join(q.Options, " | ") + "]"
	}
	if q.Recommended != "" {
		text += " (recommends: " + q.Recommended + ")"
	}
	return text
}

// askNotice is the notification a question raises: who asks and what, in a line. An agent in
// foreman ask shows as working, so nothing else says outside the queue that it waits on me.
func askNotice(key string, q Question, proceeds bool) string {
	text := strings.Join(strings.Fields(q.Text), " ")
	if proceeds {
		return fmt.Sprintf("%s went on with “%s”: %s", key, q.Recommended, text)
	}
	return key + " asks: " + text
}

// askRuns is whether the foreman ask with that pid still runs. Once it is gone, with its session
// closed or its turn interrupted, nothing reads an answer left in the state file.
var askRuns = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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

// askerKey is the dispatch asking from the pane block, which Rex names in REX_BLOCK: the one whose
// agent runs in that pane, else the one whose directory holds cwd. By the pane first: a ticket's
// agent and the one fixing its PR may share a worktree.
func askerKey(s State, block, cwd string) (string, error) {
	if block != "" {
		for key, d := range s.Dispatched {
			if d.Session != "" && s.Panes[d.Session] == block {
				return key, nil
			}
		}
	}
	cwd = filepath.Clean(cwd)
	for key, d := range s.Dispatched {
		if cwd == d.Dir || strings.HasPrefix(cwd, d.Dir+string(filepath.Separator)) {
			return key, nil
		}
	}
	return "", errors.New("not a dispatched agent's session or worktree; pass --key")
}

func runAsk(argv []string, stdin io.Reader, out io.Writer) error {
	fs := newFlags("ask")
	key := fs.String("key", "", "")
	wait := fs.Duration("timeout", askWait, "")
	var options stringList
	fs.Var(&options, "option", "")
	recommend := fs.String("recommend", "", "")
	proceed := fs.Bool("proceed", false, "")
	words, err := parseFlags(fs, argv, -1)
	if err != nil {
		return err
	}
	text := strings.Join(words, " ")
	if text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return usageError(errors.New("ask takes the question"))
	}
	q, err := newQuestion(text, options, *recommend, *proceed)
	if err != nil {
		return err
	}
	s := loadState()
	if *key == "" {
		cwd, _ := os.Getwd()
		if *key, err = askerKey(s, os.Getenv("REX_BLOCK"), cwd); err != nil {
			return err
		}
	}
	if *proceed {
		if err := decide(*key, q); err != nil {
			return err
		}
		_ = notify("foreman", askNotice(*key, q, true))
		fmt.Fprintf(out, "Go on with “%s”. The question is in my queue; if I decide otherwise, my answer comes as a message.\n", q.Recommended)
		return nil
	}
	_ = notify("foreman", askNotice(*key, q, false))
	answer, err := ask(*key, q, *wait, time.Second, time.Sleep, typedInto(s.Dispatched[*key].Dir))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, answer)
	return nil
}

// decide records a question the agent goes on without: it took its own recommendation, and the
// queue keeps the question until I confirm that or answer otherwise.
func decide(key string, q Question) error {
	q.At = time.Now()
	if _, err := updateState(func(s *State) { s.Decisions[key] = append(s.Decisions[key], q) }); err != nil {
		return err
	}
	_ = appendLog(key, logEntry{At: q.At, By: byAgent, Kind: "decision", Text: askedText(q)})
	return nil
}

// typedInto watches the Claude Code conversation in dir for what I type into it from now on: the
// function it returns gives the first such message, once there is one. The agent runs foreman ask
// inside its turn, so what I type meanwhile is queued, and Claude Code notes each queued message in
// the conversation as it is typed. The conversation is the newest in dir, which the agent wrote to
// just before asking; only what is added to it after this call is read. pi keeps no such note.
func typedInto(dir string) func() (string, bool) {
	none := func() (string, bool) { return "", false }
	if dir == "" {
		return none
	}
	files, _ := filepath.Glob(filepath.Join(claudeProjectDir(dir), "*.jsonl"))
	newest, newestMod, from := "", time.Time{}, int64(0)
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(newestMod) {
			newest, newestMod, from = f, fi.ModTime(), fi.Size()
		}
	}
	if newest == "" {
		return none
	}
	return func() (string, bool) {
		f, err := os.Open(newest)
		if err != nil {
			return "", false
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return "", false
		}
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				// A line still being written is read whole at the next look.
				return "", false
			}
			from += int64(len(line))
			if !bytes.Contains(line, []byte(`"queue-operation"`)) {
				continue
			}
			var e struct{ Type, Operation, Content string }
			if json.Unmarshal(line, &e) == nil && e.Type == "queue-operation" && e.Operation == "enqueue" && typedByMe(e.Content) {
				return strings.TrimSpace(e.Content), true
			}
		}
	}
}

// queuedTag starts what Claude Code queues on its own: an event, a task's notification, an agent's message.
var queuedTag = regexp.MustCompile(`^<[a-zA-Z][a-zA-Z-]*[ >]`)

// typedByMe is whether a queued message is one I typed rather than one Claude Code queued itself.
func typedByMe(content string) bool {
	content = strings.TrimSpace(content)
	return content != "" && !queuedTag.MatchString(content)
}

// ask records the question, which the queue shows as needing me, and polls until it is answered,
// in the queue or by what typed gives, a message I typed into the agent's session, or wait runs out.
func ask(key string, q Question, wait, poll time.Duration, sleep func(time.Duration), typed func() (string, bool)) (string, error) {
	q.At, q.Pid = time.Now(), os.Getpid()
	if _, err := updateState(func(s *State) { s.Questions[key] = q }); err != nil {
		return "", err
	}
	_ = appendLog(key, logEntry{At: q.At, By: byAgent, Kind: "question", Text: askedText(q)})
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
		if said, ok := typed(); ok {
			_, _ = updateState(func(s *State) { clearQuestion(s, key, q.At) })
			_ = appendLog(key, logEntry{By: byMe, Kind: "answer", Text: "(typed in its session) " + said})
			return "I answered in your session instead of the queue:\n\n" + said + "\n\nThe same message reaches you after this command: it is one answer, not two.", nil
		}
	}
	// The question stays in the queue, marked as one nothing waits on. An answer that came since
	// the last look is taken here, in the same write: after it, none is left where nothing reads it.
	answer := ""
	_, _ = updateState(func(s *State) {
		got, ok := s.Questions[key]
		switch {
		case !ok || !got.At.Equal(q.At):
		case got.Answer != "":
			answer = got.Answer
			delete(s.Questions, key)
		default:
			got.GaveUp = true
			s.Questions[key] = got
		}
	})
	if answer != "" {
		return answer, nil
	}
	_ = appendLog(key, logEntry{By: byAgent, Kind: "expired", Text: "no answer in " + wait.String()})
	return "", fmt.Errorf("no answer in %s. Put the question in your report and stop to ask me; my answer will come as a message", wait)
}

func clearQuestion(s *State, key string, at time.Time) {
	if q, ok := s.Questions[key]; ok && q.At.Equal(at) {
		delete(s.Questions, key)
	}
}

// settle drops a question that needs no answer any more: the one the agent asked, or one it went
// on without.
func settle(s *State, key string, at time.Time) {
	clearQuestion(s, key, at)
	left := slices.DeleteFunc(s.Decisions[key], func(q Question) bool { return q.At.Equal(at) })
	if len(left) == 0 {
		delete(s.Decisions, key)
		return
	}
	s.Decisions[key] = left
}

// confirm takes what the agent went on with as my answer to each of the questions: they leave the
// queue, and nothing is sent.
func confirm(key string, qs []Question, by string) (State, error) {
	s, err := updateState(func(s *State) {
		for _, q := range qs {
			settle(s, key, q.At)
		}
	})
	if err != nil {
		return s, err
	}
	for _, q := range qs {
		_ = appendLog(key, logEntry{By: by, Kind: "confirm", Text: q.Recommended})
	}
	return s, nil
}

// answerMessage is my answer as the agent reads it when nothing waits on it in foreman ask: it
// comes as a message, perhaps long after the question, so it says which one it answers.
func answerMessage(q Question, decision bool, text string) string {
	asked := strings.Join(strings.Fields(q.Text), " ")
	if decision {
		return fmt.Sprintf("You asked me “%s” and went on with “%s”. My answer is different: %s", asked, q.Recommended, text)
	}
	return fmt.Sprintf("You asked me “%s”. My answer: %s", asked, text)
}

// answerItem gives the agent my answer to its pending question. One that waits in foreman ask
// reads it there. Otherwise nothing waits on it, and it goes to the agent as a message: typed into
// its pane, or for a closed session, as the turn the resumed agent starts with. Only I resume one.
// The answer an agent went on with confirms it, and nothing is sent.
func answerItem(s State, it queueItem, text, by string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("nothing to answer with")
	}
	q, decision, ok := it.pendingQuestion()
	if !ok {
		return "", fmt.Errorf("%s has no question waiting", it.Key)
	}
	if asking(it) {
		if err := answerQuestion(it.Key, text, by); err != nil {
			return "", err
		}
		return "✓ Answered " + it.Key, nil
	}
	// The answer it already went on with: nothing to tell it.
	if decision && strings.EqualFold(text, q.Recommended) {
		if _, err := confirm(it.Key, []Question{q}, by); err != nil {
			return "", err
		}
		return "✓ Confirmed what " + it.Key + " went on with", nil
	}
	msg, said := answerMessage(q, decision, text), ""
	switch {
	case it.Agent != "":
		if err := replyAllowed(it, true, by); err != nil {
			return "", err
		}
		if err := sendToAgent(it.Session, it.Block, msg); err != nil {
			return "", fmt.Errorf("answer to %s: %w", it.Key, err)
		}
		said = "✓ Sent " + it.Key + " the answer in its pane"
	case by != byMe:
		return "", fmt.Errorf("%s's session is closed, and only I resume an agent: f on its row in the queue answers it", it.Key)
	default:
		if _, err := resumeAgent(s, it.Key, it.Title, it.Dir, msg); err != nil {
			return "", fmt.Errorf("resuming %s: %w", it.Key, err)
		}
		said = "✓ Resumed " + it.Key + " with the answer"
	}
	_, _ = updateState(func(s *State) { settle(s, it.Key, q.At) })
	_ = appendLog(it.Key, logEntry{By: by, Kind: "answer", Text: text})
	return said, nil
}

// questionLines head an agent's detail with what it asked: the question it waits on or left
// behind, then those it went on without, each with its choices. The keys named answer the first.
func questionLines(it queueItem) []string {
	var out []string
	if q := it.Question; q != nil {
		switch {
		case !it.Left:
			out = append(out, "# Waiting on an answer", "")
		case it.Agent == "":
			out = append(out, "# Left a question", "", "Its session closed before I answered. An answer resumes it, as the turn it starts with.", "")
		default:
			out = append(out, "# Left a question", "", "It no longer waits in `foreman ask`: an answer is typed into its pane.", "")
		}
		out = append(append(out, q.Text, ""), choiceLines(*q, false)...)
		hint := answerHint(*q, false)
		if it.Left {
			hint += " `x` sets it aside, answered elsewhere."
		}
		out = append(out, hint, "")
	}
	for n, q := range it.Decisions {
		out = append(append(out, "# Went on without an answer", "", q.Text, ""), choiceLines(q, true)...)
		if n == 0 && it.Question == nil {
			out = append(out, answerHint(q, true), "")
		}
	}
	return out
}

// choiceLines are the question's choices, numbered as the digits that answer with them, with the
// one the agent recommends, or went on with, marked.
func choiceLines(q Question, decision bool) []string {
	mark, alone := "recommended", "It would go with: "
	if decision {
		mark, alone = "went on with this", "It went on with: "
	}
	if len(q.Options) == 0 {
		if q.Recommended == "" {
			return nil
		}
		return []string{alone + "**" + q.Recommended + "**", ""}
	}
	var out []string
	for n, o := range q.Options {
		line := fmt.Sprintf("%d. %s", n+1, o)
		if o == q.Recommended {
			line += " **(" + mark + ")**"
		}
		out = append(out, line)
	}
	return append(out, "")
}

// answerHint names the keys that answer the question.
func answerHint(q Question, decision bool) string {
	digits := ""
	switch n := len(q.Options); {
	case n == 1:
		digits = "`1`"
	case n > 1:
		digits = fmt.Sprintf("`1`–`%d`", n)
	}
	switch {
	case decision && digits != "":
		return "`a` confirms it, " + digits + " or `f` send another answer; `x` confirms everything it went on with."
	case decision:
		return "`a` confirms it, `f` sends another answer; `x` confirms everything it went on with."
	case digits != "" && q.Recommended != "":
		return digits + " answer with a choice, `a` with the recommended one, `f` in your own words."
	case digits != "":
		return digits + " answer with a choice, `f` in your own words."
	case q.Recommended != "":
		return "`a` answers with that, `f` in your own words."
	}
	return "`f` answers it."
}

// answerQuestion answers the question the dispatch waits on in foreman ask, which reads the
// answer from the state file.
func answerQuestion(key, text, by string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("nothing to answer with")
	}
	found, gaveUp := false, false
	if _, err := updateState(func(s *State) {
		q, ok := s.Questions[key]
		if gaveUp = ok && q.GaveUp; gaveUp {
			return
		}
		if !ok || q.Answer != "" {
			return
		}
		found = true
		q.Answer, q.By = text, by
		s.Questions[key] = q
	}); err != nil {
		return err
	}
	if gaveUp {
		return fmt.Errorf("%s stopped waiting for the answer just now; answer again, and it goes to the agent as a message", key)
	}
	if !found {
		return fmt.Errorf("%s has no question waiting", key)
	}
	return appendLog(key, logEntry{By: by, Kind: "answer", Text: text})
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
	s, it, err := lookup(fs.Arg(0))
	if err != nil {
		return err
	}
	said, err := answerItem(s, it, text, role())
	if err != nil {
		return err
	}
	fmt.Fprintln(out, said)
	return nil
}

// logLines renders the log for the queue's detail pane.
func logLines(entries []logEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	verbs := map[string]string{"question": "asks", "decision": "asks, and goes on", "answer": "answers", "confirm": "confirms", "reply": "sends", "expired": "gave up", "aside": "sets the question aside", "review": "sends"}
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
