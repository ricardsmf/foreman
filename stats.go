package main

// Stats: what the agents foreman started used, read from the conversations Claude Code and pi keep
// for the directory each one worked in. Nothing is recorded as it happens: the event log says
// which dispatch a directory was at any time, and the conversations say the rest.

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A tally is what agents used: tokens by kind, what they cost at API prices, and the time the
// agents worked.
type tally struct {
	In         int64   `json:"in"`
	Out        int64   `json:"out"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Cost       float64 `json:"cost"`
	// Unpriced counts the tokens of models foreman has no price for, which Cost leaves out.
	Unpriced int64   `json:"unpriced,omitempty"`
	Seconds  float64 `json:"agentSeconds"`
}

func (t *tally) add(o tally) {
	t.In += o.In
	t.Out += o.Out
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Cost += o.Cost
	t.Unpriced += o.Unpriced
	t.Seconds += o.Seconds
}

func (t tally) tokens() int64 { return t.In + t.Out + t.CacheRead + t.CacheWrite }

// A price is what a model's tokens cost on Anthropic's API, in dollars per million: input, output
// and cache reads. A cache write costs 1.25 times the input when kept five minutes, twice when
// kept an hour.
type price struct{ in, out, cacheRead float64 }

// prices go by the start of a model's ID, a longer ID before the one it starts with. Haiku 5.5's
// is the one for prompts up to 100k tokens.
var prices = []struct {
	model string
	price
}{
	{"claude-fable-5-2", price{8, 40, 0.20}},
	{"claude-mythos-5-2", price{8, 40, 0.20}},
	{"claude-fable-5-1", price{10, 50, 0.25}},
	{"claude-mythos-5-1", price{10, 50, 0.25}},
	{"claude-fable-5", price{10, 50, 1}},
	{"claude-mythos-5", price{10, 50, 1}},
	{"claude-opus-5-5", price{4, 20, 0.20}},
	{"claude-opus-5", price{5, 25, 0.50}},
	{"claude-opus-4-8", price{5, 25, 0.50}},
	{"claude-opus-4-7", price{5, 25, 0.50}},
	{"claude-opus-4-6", price{5, 25, 0.50}},
	{"claude-sonnet-5-5", price{2, 10, 0.10}},
	{"claude-sonnet-5", price{2, 10, 0.20}},
	{"claude-sonnet-4-6", price{3, 15, 0.30}},
	{"claude-haiku-5-5", price{0.10, 0.50, 0.01}},
	{"claude-haiku-4-5", price{1, 5, 0.10}},
}

func priceOf(model string) (price, bool) {
	for _, p := range prices {
		if strings.HasPrefix(model, p.model) {
			return p.price, true
		}
	}
	return price{}, false
}

// A sample is what one message used, or a stretch of an agent's work, and when.
type sample struct {
	at time.Time
	id string // the message's, "" for a stretch of work
	tally
}

// maxGap is the longest an agent goes between two entries of a turn: past it the machine slept
// or the agent hung, and the gap isn't time it worked.
const maxGap = 30 * time.Minute

// workSample is the stretch of work that ends at an entry written at at, after the one at prev:
// none for a turn's first entry, which follows a wait for me, or over a gap too long to be work.
func workSample(prev, at time.Time, prompt bool) (sample, bool) {
	gap := at.Sub(prev)
	if prev.IsZero() || prompt || gap <= 0 || gap > maxGap {
		return sample{}, false
	}
	return sample{at: at, tally: tally{Seconds: gap.Seconds()}}, true
}

// eachLine calls fn with every line of r. ReadBytes, not a Scanner: a line can hold a whole API
// request, past any buffer worth fixing.
func eachLine(r io.Reader, fn func(line []byte)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if err != nil {
			return
		}
	}
}

// claudeSamples reads a Claude Code conversation: each message's tokens once, priced by its
// model, and with timed the stretches the agent worked, from each prompt of mine to the last
// entry before the next. A subagent's conversation isn't timed: it works while its caller waits.
func claudeSamples(r io.Reader, timed bool) []sample {
	var out []sample
	byID := map[string]int{}
	var prev time.Time
	eachLine(r, func(line []byte) {
		if !bytes.Contains(line, []byte(`"assistant"`)) && !bytes.Contains(line, []byte(`"user"`)) {
			return
		}
		var e struct {
			Type      string    `json:"type"`
			IsMeta    bool      `json:"isMeta"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				ID      string          `json:"id"`
				Model   string          `json:"model"`
				Content json.RawMessage `json:"content"`
				Usage   *struct {
					In            int64 `json:"input_tokens"`
					Out           int64 `json:"output_tokens"`
					CacheRead     int64 `json:"cache_read_input_tokens"`
					CacheWrite    int64 `json:"cache_creation_input_tokens"`
					CacheCreation struct {
						FiveMinutes int64 `json:"ephemeral_5m_input_tokens"`
						Hour        int64 `json:"ephemeral_1h_input_tokens"`
					} `json:"cache_creation"`
					Speed string `json:"speed"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Timestamp.IsZero() || (e.Type != "assistant" && e.Type != "user") {
			return
		}
		if timed {
			prompt := e.Type == "user" && !e.IsMeta && typedText(e.Message.Content) != ""
			if s, ok := workSample(prev, e.Timestamp, prompt); ok {
				out = append(out, s)
			}
			prev = e.Timestamp
		}
		// Claude Code's own notes carry a model that isn't one, and no tokens.
		u := e.Message.Usage
		if e.Type != "assistant" || u == nil || e.Message.Model == "<synthetic>" {
			return
		}
		t := tally{In: u.In, Out: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
		if p, ok := priceOf(e.Message.Model); ok {
			short, long := u.CacheCreation.FiveMinutes, u.CacheCreation.Hour
			if short+long == 0 {
				short = u.CacheWrite
			}
			t.Cost = (float64(u.In)*p.in + float64(u.Out)*p.out + float64(u.CacheRead)*p.cacheRead +
				float64(short)*p.in*1.25 + float64(long)*p.in*2) / 1e6
			// Fast mode is priced at twice the standard rate.
			if u.Speed == "fast" {
				t.Cost *= 2
			}
		} else {
			t.Unpriced = t.tokens()
		}
		// A message is written once per block of its content, with the tokens it had by then.
		if n, seen := byID[e.Message.ID]; seen && e.Message.ID != "" {
			if t.Out >= out[n].Out {
				out[n].tally = t
			}
			return
		}
		byID[e.Message.ID] = len(out)
		out = append(out, sample{at: e.Timestamp, id: e.Message.ID, tally: t})
	})
	return out
}

// piSamples reads a pi session the same way. pi writes what each message cost itself.
func piSamples(r io.Reader, timed bool) []sample {
	var out []sample
	var prev time.Time
	eachLine(r, func(line []byte) {
		if !bytes.Contains(line, []byte(`"message"`)) {
			return
		}
		var e struct {
			Type      string    `json:"type"`
			ID        string    `json:"id"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Role  string `json:"role"`
				Usage *struct {
					In         int64 `json:"input"`
					Out        int64 `json:"output"`
					CacheRead  int64 `json:"cacheRead"`
					CacheWrite int64 `json:"cacheWrite"`
					Cost       struct {
						Total float64 `json:"total"`
					} `json:"cost"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "message" || e.Timestamp.IsZero() || e.Message.Role == "system" {
			return
		}
		if timed {
			if s, ok := workSample(prev, e.Timestamp, e.Message.Role == "user"); ok {
				out = append(out, s)
			}
			prev = e.Timestamp
		}
		if u := e.Message.Usage; e.Message.Role == "assistant" && u != nil {
			out = append(out, sample{at: e.Timestamp, id: e.ID, tally: tally{
				In: u.In, Out: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Cost: u.Cost.Total}})
		}
	})
	return out
}

// sessionFiles keeps what each conversation file gave, until the file changes: the stats view reads
// them again and again, and a conversation runs to megabytes.
var sessionFiles = struct {
	sync.Mutex
	files map[string]sessionFile
}{files: map[string]sessionFile{}}

type sessionFile struct {
	size    int64
	mod     time.Time
	samples []sample
}

// fileSamples is what the conversation in file used since since: none when it was last written
// before then.
func fileSamples(file string, since time.Time, read func(io.Reader, bool) []sample, timed bool) []sample {
	st, err := os.Stat(file)
	if err != nil || st.ModTime().Before(since) {
		return nil
	}
	sessionFiles.Lock()
	kept, ok := sessionFiles.files[file]
	sessionFiles.Unlock()
	if ok && kept.size == st.Size() && kept.mod.Equal(st.ModTime()) {
		return kept.samples
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	samples := read(f, timed)
	sessionFiles.Lock()
	sessionFiles.files[file] = sessionFile{st.Size(), st.ModTime(), samples}
	sessionFiles.Unlock()
	return samples
}

// dirSamples is what the agents that worked in dir used since since: Claude Code's conversations
// there, with their subagents', and pi's sessions. A message that two conversations hold, as a
// forked one does, counts once.
func dirSamples(dir string, since time.Time) []sample {
	var out []sample
	seen := map[string]bool{}
	take := func(pattern string, read func(io.Reader, bool) []sample, timed bool) {
		files, _ := filepath.Glob(pattern)
		for _, f := range files {
			for _, s := range fileSamples(f, since, read, timed) {
				if s.id != "" {
					if seen[s.id] {
						continue
					}
					seen[s.id] = true
				}
				out = append(out, s)
			}
		}
	}
	claude := claudeProjectDir(dir)
	take(filepath.Join(claude, "*.jsonl"), claudeSamples, true)
	take(filepath.Join(claude, "*", "subagents", "*.jsonl"), claudeSamples, false)
	take(filepath.Join(piSessionDir(dir), "*.jsonl"), piSamples, true)
	return out
}

// An agentRun is an agent foreman started, as the stats count it: a dispatch, a watcher or the
// orchestrator, where it worked and from when. A zero From takes all its directory holds.
type agentRun struct {
	Key, Title, Repo, Dir string
	From                  time.Time
}

// statRuns are the runs to count: every dispatch the log has, those from before it that are
// still dispatched, the watchers and the orchestrator. A dispatch without a worktree is left
// out: it shares its repo's main checkout with whatever else I run there.
func statRuns(s State, events []logEntry) []agentRun {
	var runs []agentRun
	for _, e := range events {
		if e.Kind != kindDispatched || e.Dir == "" || e.Dir == e.Repo {
			continue
		}
		repo := repoOf(e.Key, e.Dir)
		if e.Repo != "" {
			repo = filepath.Base(e.Repo)
		}
		runs = append(runs, agentRun{e.Key, e.Title, repo, e.Dir, e.At})
	}
	for key, d := range s.Dispatched {
		if d.ID == "" && d.Dir != "" && mainCheckout(d.Dir) != d.Dir {
			runs = append(runs, agentRun{key, d.Title, repoOf(key, d.Dir), d.Dir, time.Time{}})
		}
	}
	for _, w := range watchers {
		runs = append(runs, agentRun{w.Name, "watcher", repoOf("", w.Dir), w.Dir, time.Time{}})
	}
	return append(runs, agentRun{"orchestrator", "orchestrator", "", orchestratorDir, time.Time{}})
}

// runTallies is what each run used since since, by its place in runs. What a directory holds
// goes to the run that had started there last: a key dispatched again reuses its worktree.
func runTallies(runs []agentRun, since time.Time) []tally {
	out := make([]tally, len(runs))
	byDir := map[string][]int{}
	for n, r := range runs {
		byDir[r.Dir] = append(byDir[r.Dir], n)
	}
	for dir, ns := range byDir {
		slices.SortFunc(ns, func(a, b int) int { return runs[a].From.Compare(runs[b].From) })
		for _, s := range dirSamples(dir, since) {
			if s.at.Before(since) {
				continue
			}
			owner := -1
			for _, n := range ns {
				if !runs[n].From.After(s.at) {
					owner = n
				}
			}
			if owner >= 0 {
				out[owner].add(s.tally)
			}
		}
	}
	return out
}

// A statRow is a line of the stats: a dispatch, a watcher or the orchestrator, or with them
// grouped, a repo.
type statRow struct {
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	Repo  string `json:"repo,omitempty"`
	tally
}

type statsReport struct {
	Days  int       `json:"days"`
	Since time.Time `json:"since"`
	By    string    `json:"by"` // ticket or repo
	Total tally     `json:"total"`
	// What the log says ended in those days: tickets done, and PRs of mine merged.
	TicketsDone int       `json:"ticketsDone"`
	PrsMerged   int       `json:"prsMerged"`
	Rows        []statRow `json:"rows"`
}

// daysBack is the start of the day days-1 days before now's: 1 is today.
func daysBack(now time.Time, days int) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d-max(1, days)+1, 0, 0, 0, 0, now.Location())
}

const (
	byTicket = "ticket"
	byRepo   = "repo"
)

// statsOf counts the last days days, a row per dispatch or per repo, the costliest first.
func statsOf(s State, events []logEntry, days int, by string, now time.Time) statsReport {
	rep := statsReport{Days: max(1, days), Since: daysBack(now, days), By: by, Rows: []statRow{}}
	runs := statRuns(s, events)
	rows := map[string]*statRow{}
	for n, t := range runTallies(runs, rep.Since) {
		if t.tokens() == 0 && t.Seconds == 0 {
			continue
		}
		r := runs[n]
		row := statRow{Name: r.Key, Title: r.Title, Repo: r.Repo}
		if by == byRepo {
			row = statRow{Name: cmp.Or(r.Repo, r.Key)}
		}
		if rows[row.Name] == nil {
			rows[row.Name] = &row
		}
		rows[row.Name].add(t)
		rep.Total.add(t)
	}
	for _, r := range rows {
		rep.Rows = append(rep.Rows, *r)
	}
	slices.SortFunc(rep.Rows, func(a, b statRow) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), cmp.Compare(b.tokens(), a.tokens()), strings.Compare(a.Name, b.Name))
	})
	for _, e := range events {
		switch {
		case e.At.Before(rep.Since):
		case e.Kind == "ticket-done":
			rep.TicketsDone++
		case e.Kind == "pr-merged" && !reviewKey(e.Key):
			rep.PrsMerged++
		}
	}
	return rep
}

// money is a cost in dollars, to the cent.
func money(c float64) string {
	if c > 0 && c < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", c)
}

// tokenCount is a number of tokens in three figures: 950, 81.2k, 48.2M.
func tokenCount(n int64) string {
	switch {
	case n >= 999_500:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e6)) + "M"
	case n >= 1000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e3)) + "k"
	}
	return fmt.Sprint(n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// workTime is a time worked, to the minute: 3h 12m, 45m, <1m.
func workTime(seconds float64) string {
	m := int(math.Round(seconds / 60))
	switch {
	case seconds <= 0:
		return "0m"
	case m < 1:
		return "<1m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

// periodName names the days a report covers.
func periodName(days int) string {
	if days <= 1 {
		return "today"
	}
	return fmt.Sprintf("%d days", days)
}

// statsHead is the report's totals in a line.
func statsHead(rep statsReport) []string {
	head := []string{periodName(rep.Days), money(rep.Total.Cost), tokenCount(rep.Total.tokens()) + " tokens",
		workTime(rep.Total.Seconds) + " of agent time",
		plural(rep.TicketsDone, "ticket") + " done", plural(rep.PrsMerged, "PR") + " merged"}
	return head
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func runStats(argv []string, out io.Writer) error {
	fs := newFlags("stats")
	days := fs.Int("days", 1, "")
	by := fs.String("by", byTicket, "")
	asJSON := fs.Bool("json", false, "")
	if _, err := parseFlags(fs, argv, 0); err != nil {
		return err
	}
	if *days < 1 {
		return usageError(fmt.Errorf("--days counts back from today, which is 1, so not %d", *days))
	}
	if *by != byTicket && *by != byRepo {
		return usageError(fmt.Errorf("--by is ticket or repo, not %q", *by))
	}
	rep := statsOf(loadState(), readEvents(), *days, *by, time.Now())
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeStats(out, rep)
}

// writeStats prints the totals, then a row a line: name, cost, tokens, time worked, title.
func writeStats(out io.Writer, rep statsReport) error {
	if _, err := fmt.Fprintln(out, strings.Join(statsHead(rep), " · ")); err != nil {
		return err
	}
	nameW := 4
	for _, r := range rep.Rows {
		nameW = max(nameW, len(r.Name))
	}
	for _, r := range rep.Rows {
		if _, err := fmt.Fprintf(out, "%-*s  %8s  %7s  %7s  %s\n", nameW, r.Name, money(r.Cost), tokenCount(r.tokens()), workTime(r.Seconds), r.Title); err != nil {
			return err
		}
	}
	if rep.Total.Unpriced > 0 {
		_, err := fmt.Fprintf(out, "%s tokens are on models foreman has no price for; the cost leaves them out\n", tokenCount(rep.Total.Unpriced))
		return err
	}
	return nil
}
