package main

// What the queue tells Rex and does in an agent's pane: its status records, replies and key presses.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// rexStatus is the queue's report to Rex in the Program Status Protocol (OSC 7501), which marks
// the foreman session in the session picker whatever session is showing: blocked while something
// asks for me, done while a report or a PR waits to be read, idle otherwise. Agents at work
// aren't reported, since their own sessions show that.
func rexStatus(needs, review int) string {
	state, msg := "idle", ""
	switch {
	case needs > 0:
		state, msg = "blocked", fmt.Sprintf("%d need you", needs)
	case review > 0:
		state, msg = "done", fmt.Sprintf("%d to review", review)
	}
	report := "\x1b]7501;state=" + state + ":app=foreman"
	if msg != "" {
		report += ":msg=" + base64.StdEncoding.EncodeToString([]byte(msg))
	}
	return report + "\x1b\\"
}

// oneLine is text as a Program Status value takes it: no control characters, at most limit bytes.
func oneLine(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, text)
	for len(text) > limit {
		_, size := utf8.DecodeLastRuneInString(text)
		text = text[:len(text)-size]
	}
	return text
}

var recordIDRe = regexp.MustCompile(`[^A-Za-z0-9_.+-]+`)

// itemRecords are the reports of the items that wait on me, each a record of its own under the
// queue's, by record id: blocked with what it waits for, or done. Rex lists a terminal's records
// one by one wherever it shows them.
func itemRecords(items []queueItem) map[string]string {
	out := map[string]string{}
	for _, it := range items {
		if !it.pending() {
			continue
		}
		id := it.Key
		state, kind, msg := "done", "", "report ready"
		switch {
		case it.Kind == prItem:
			id, msg = "pr-"+id, "PR "+it.Pr.Status
			if it.urgent() {
				state = "blocked"
			}
		case it.Question != nil && it.Question.Escalated != "":
			state, kind, msg = "blocked", "question", "asks: "+it.Question.Text
		case it.Agent == "blocked":
			state, kind, msg = "blocked", "permission", "waits on a prompt"
		case it.Agent == "error":
			state, msg = "error", "failed"
		}
		id = oneLine(recordIDRe.ReplaceAllString(id, "-"), 32)
		report := "\x1b]7501;state=" + state + ":id=" + id + ":app=foreman"
		if kind != "" {
			report += ":kind=" + kind
		}
		title := it.Key
		if it.Title != "" {
			title += ": " + it.Title
		}
		out[id] = report + ":title=" + base64.StdEncoding.EncodeToString([]byte(oneLine(title, 180))) +
			":msg=" + base64.StdEncoding.EncodeToString([]byte(oneLine(msg, 1000))) + "\x1b\\"
	}
	return out
}

// report tells Rex what changed: the queue's status, and the record of each item that waits on me.
func (q *queueModel) report() tea.Cmd {
	if os.Getenv("REX_SESSION") == "" {
		return nil
	}
	var out strings.Builder
	if status := rexStatus(q.count(needsYou), q.count(toReview)); status != q.reported {
		q.reported = status
		out.WriteString(status)
	}
	want := itemRecords(q.all)
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if q.records[id] != want[id] {
			out.WriteString(want[id])
		}
	}
	for _, id := range slices.Sorted(maps.Keys(q.records)) {
		if _, ok := want[id]; !ok {
			out.WriteString("\x1b]7501;state=clear:id=" + id + "\x1b\\")
		}
	}
	q.records = want
	if out.Len() == 0 {
		return nil
	}
	return tea.Raw(out.String())
}

// inputBoxReach is how many lines up from the end of a pane its input box may start: the box,
// taller when text is typed in it, and the footer under it.
const inputBoxReach = 16

// withoutInputBox is an agent's screen without the input box Claude Code ends it with: two rules
// around the prompt, and a footer. What is left ends on what the agent last wrote. A screen that
// doesn't end that way is returned as it is.
func withoutInputBox(lines []string) []string {
	var rules []int
	for n := len(lines) - 1; n >= max(0, len(lines)-inputBoxReach) && len(rules) < 2; n-- {
		if isRule(lines[n]) {
			rules = append(rules, n)
		}
	}
	if len(rules) < 2 {
		return lines
	}
	end := rules[1]
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// promptDetail is what a blocked agent's pane asks, to head its detail, and the seen token of the
// pane it was read from: y and n press a key there only while the pane still shows it.
func promptDetail(it queueItem, capture func(queueItem) (string, error)) ([]string, string) {
	screen, err := capture(it)
	if err != nil {
		return nil, ""
	}
	out := []string{"# It waits on this", "", "```"}
	out = append(out, paneAsks(screen)...)
	return append(out, "```", "", "`y` presses enter in its pane and `n` presses esc, while it still shows this. `enter` goes there.", ""),
		seenToken(it.Agent, screen)
}

// pressPrompt presses key in the agent's pane, unless the pane no longer shows what seen stands
// for: the prompt was answered meanwhile, and the key would land on whatever replaced it.
func pressPrompt(it queueItem, seen, key string) error {
	if err := stillSeen(it, seen, capturePane); err != nil {
		return fmt.Errorf("%s's pane changed since it was read, so nothing was pressed; it is read again", it.Key)
	}
	block, err := agentBlock(it.Session)
	if err != nil {
		return err
	}
	_, err = sh("", "rex", "send-key", "-s", it.Session, "-b", block, key)
	return err
}

// agentBlockScript finds the session's pane whose program reports a status (pi or Claude Code),
// else its first placed pane: a session can hold a shell split beside the agent.
const agentBlockScript = attachLua + `
local label = %s
for _, s in ipairs(rex.call("session.list").sessions or {}) do
  if s.session_id == label or s.label == label then
    attach(s.session_id)
    local first
    for _, b in ipairs((rex.call("session.list_blocks", {session_id = s.session_id}) or {}).blocks or {}) do
      if b.placement == "placed" then
        local ok, st = pcall(rex.call, "block.method", {session_id = s.session_id, block_id = b.block_id, method = "program_status"})
        if ok and st and #(st.records or {}) > 0 then return b.block_id end
        first = first or b.block_id
      end
    end
    if first then return first end
    error("no pane in session " .. label, 0)
  end
end
error("no session " .. label, 0)
`

// agentBlock is the session's pane where the agent runs.
func agentBlock(session string) (string, error) {
	out, err := sh("", "rex", "do", "--all", "-e", fmt.Sprintf(agentBlockScript, strconv.Quote(session)), "--json")
	if err != nil {
		return "", err
	}
	var block string
	if err := json.Unmarshal([]byte(out), &block); err != nil {
		return "", fmt.Errorf("finding the agent's pane: %w", err)
	}
	return block, nil
}

// sendToAgent types text into the agent's pane and presses enter, as if I had replied there.
func sendToAgent(session, text string) error {
	block, err := agentBlock(session)
	if err != nil {
		return err
	}
	// After --: a reply that starts with a dash isn't a flag.
	if _, err := sh("", "rex", "send", "-s", session, "-b", block, "--", text); err != nil {
		return err
	}
	// A separate key press: in one write the agent's editor takes the newline as pasted text.
	time.Sleep(150 * time.Millisecond)
	_, err = sh("", "rex", "send-key", "-s", session, "-b", block, "enter")
	return err
}
