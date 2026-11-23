package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempHome moves home, where the agents keep their conversations, to a temp dir, with no
// watcher and the orchestrator under it.
func tempHome(t *testing.T) {
	t.Helper()
	oldHome, oldOrchestrator, oldWatchers := home, orchestratorDir, watchers
	home = t.TempDir()
	orchestratorDir, watchers = filepath.Join(home, "orchestrator"), nil
	t.Cleanup(func() { home, orchestratorDir, watchers = oldHome, oldOrchestrator, oldWatchers })
}

func putLines(t *testing.T, file string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// claudeMsg is an assistant message as Claude Code writes it: write is kept an hour.
func claudeMsg(at time.Time, id, model string, in, out, read, write int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_1h_input_tokens":%d,"ephemeral_5m_input_tokens":0}}}}`,
		at.Format(time.RFC3339Nano), id, model, in, out, read, write, write)
}

func claudePrompt(at time.Time) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":"go on"}}`, at.Format(time.RFC3339Nano))
}

func claudeToolResult(at time.Time) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","content":"ok"}]}}`, at.Format(time.RFC3339Nano))
}

func sum(samples []sample) tally {
	var t tally
	for _, s := range samples {
		t.add(s.tally)
	}
	return t
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A Claude Code conversation gives each message's tokens once, priced by its model, and the
// time the agent worked: not the wait for my next prompt, nor a gap too long to be work.
func TestClaudeSamples(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	lines := strings.Join([]string{
		claudePrompt(at(0)),
		// Written twice, a block of its content each: the tokens it ended with count, once.
		claudeMsg(at(10), "a", "claude-opus-5-5", 100, 10, 1000, 2000),
		claudeMsg(at(12), "a", "claude-opus-5-5", 100, 50, 1000, 2000),
		claudeToolResult(at(60)),
		claudeMsg(at(90), "b", "claude-fable-5-2", 10, 100, 0, 0),
		claudeMsg(at(95), "s", "<synthetic>", 0, 0, 0, 0),
		// It waited for me from 95 to 600.
		claudePrompt(at(600)),
		claudeMsg(at(630), "c", "some-other-model", 5, 5, 0, 0),
		// The machine slept for 40 minutes.
		claudeToolResult(at(3030)),
		claudeMsg(at(3040), "d", "claude-opus-5-5", 1, 1, 0, 0),
		`{"type":"api-request-blob","note":"a line that names an \"assistant\" and isn't one"}`,
		"not json",
	}, "\n")
	got := sum(claudeSamples(strings.NewReader(lines), true))
	want := tally{In: 116, Out: 156, CacheRead: 1000, CacheWrite: 2000, Unpriced: 10, Seconds: 135,
		Cost: (100*4+50*20+1000*0.2+2000*8)/1e6 + (10*8+100*40)/1e6 + (1*4+1*20)/1e6}
	if got.In != want.In || got.Out != want.Out || got.CacheRead != want.CacheRead || got.CacheWrite != want.CacheWrite ||
		got.Unpriced != want.Unpriced || got.Seconds != want.Seconds || !near(got.Cost, want.Cost) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// A subagent works while its caller waits: its tokens count, its time doesn't.
	if sub := sum(claudeSamples(strings.NewReader(lines), false)); sub.Seconds != 0 || sub.tokens() != want.tokens() {
		t.Errorf("a subagent's: %+v; want the same tokens and no time", sub)
	}
	// A cache write that doesn't say how long it is kept is one of five minutes; fast mode costs double.
	short := `{"type":"assistant","timestamp":"2026-11-20T09:00:00Z","message":{"id":"e","model":"claude-opus-5-5","usage":{"cache_creation_input_tokens":1000%s}}}`
	if got := sum(claudeSamples(strings.NewReader(fmt.Sprintf(short, "")), true)); !near(got.Cost, 1000*4*1.25/1e6) {
		t.Errorf("a five-minute write cost %v", got.Cost)
	}
	if got := sum(claudeSamples(strings.NewReader(fmt.Sprintf(short, `,"speed":"fast"`)), true)); !near(got.Cost, 2*1000*4*1.25/1e6) {
		t.Errorf("in fast mode it cost %v", got.Cost)
	}
}

// pi writes what each message cost itself.
func TestPiSamples(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"session","version":3,"id":"s","timestamp":"2026-11-12T12:00:00Z","cwd":"/w/x"}`,
		`{"type":"message","id":"sys","timestamp":"2026-11-12T11:00:00Z","message":{"role":"system","content":""}}`,
		`{"type":"message","id":"u1","timestamp":"2026-11-12T12:00:00Z","message":{"role":"user","content":"go"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-11-12T12:00:20Z","message":{"role":"assistant","model":"claude-opus-5-5","usage":{"input":2,"output":100,"cacheRead":1000,"cacheWrite":500,"cost":{"total":0.01}}}}`,
		`{"type":"message","id":"t1","timestamp":"2026-11-12T12:00:50Z","message":{"role":"toolResult"}}`,
		`{"type":"message","id":"a2","timestamp":"2026-11-12T12:01:00Z","message":{"role":"assistant","model":"claude-opus-5-5","usage":{"input":1,"output":1,"cost":{"total":0.02}}}}`,
	}, "\n")
	got := sum(piSamples(strings.NewReader(lines), true))
	if got.tokens() != 1604 || !near(got.Cost, 0.03) || got.Seconds != 60 {
		t.Errorf("got %+v; want 1604 tokens, $0.03 and 60 s", got)
	}
}

// What a directory's conversations hold goes to the dispatch that was running there at the
// time, in the days asked for, and a dispatch without a worktree is left out.
func TestStatsCountEachDispatch(t *testing.T) {
	tempState(t)
	tempHome(t)
	now := time.Now()
	today := daysBack(now, 1)
	old := today.Add(-3 * 24 * time.Hour)
	app := filepath.Join(home, "w", "app")
	wt1, wt2, wt5 := app+"-fld-1", app+"-fld-2", filepath.Join(home, "w", "other-fld-5")
	if err := writeEvents(
		logEntry{At: old, Key: "FLD-1", Dispatch: "a", Kind: kindDispatched, Title: "One", Dir: wt1, Repo: app},
		logEntry{At: old, Key: "FLD-2", Dispatch: "a", Kind: kindDispatched, Title: "Two", Dir: wt2, Repo: app},
		logEntry{At: old, Key: "task:app-2", Dispatch: "a", Kind: kindDispatched, Title: "In the main checkout", Dir: app, Repo: app},
		logEntry{At: old, Key: "FLD-3", Kind: "ticket-done"},
		logEntry{At: today.Add(time.Hour), Key: "FLD-1", Dispatch: "b", Kind: kindDispatched, Title: "One again", Dir: wt1, Repo: app},
		logEntry{At: today.Add(2 * time.Hour), Key: "FLD-1", Dispatch: "b", Kind: "ticket-done"},
		logEntry{At: today.Add(2 * time.Hour), Key: "FLD-2", Dispatch: "a", Kind: "pr-merged", Text: "app#3"},
		// A review's PR merging is someone else's.
		logEntry{At: today.Add(2 * time.Hour), Key: "app#9", Dispatch: "a", Kind: "pr-merged"},
	); err != nil {
		t.Fatal(err)
	}
	// From before the log: no dispatch id, and no event says when it started.
	dispatched(t, "FLD-5", wt5)
	const million = 1_000_000
	putLines(t, filepath.Join(claudeProjectDir(wt1), "c.jsonl"),
		claudeMsg(old.Add(-time.Hour), "before", "claude-opus-5-5", million, 0, 0, 0),
		claudeMsg(old.Add(time.Hour), "first", "claude-opus-5-5", million, 0, 0, 0),
		claudeMsg(today.Add(2*time.Hour), "second", "claude-opus-5-5", 0, 100_000, 0, 0))
	// A fork of that conversation holds its messages too.
	putLines(t, filepath.Join(claudeProjectDir(wt1), "fork.jsonl"),
		claudeMsg(today.Add(2*time.Hour), "second", "claude-opus-5-5", 0, 100_000, 0, 0))
	putLines(t, filepath.Join(claudeProjectDir(wt1), "c", "subagents", "agent-1.jsonl"),
		claudeMsg(today.Add(2*time.Hour), "sub", "claude-haiku-5-5", million, 0, 0, 0))
	putLines(t, filepath.Join(piSessionDir(wt2), "s.jsonl"),
		`{"type":"message","id":"p1","timestamp":"`+today.Add(30*time.Minute).Format(time.RFC3339)+`","message":{"role":"assistant","usage":{"input":10,"cost":{"total":1.5}}}}`)
	putLines(t, filepath.Join(claudeProjectDir(app), "mine.jsonl"),
		claudeMsg(today.Add(time.Hour), "mine", "claude-opus-5-5", million, 0, 0, 0))
	putLines(t, filepath.Join(claudeProjectDir(wt5), "c.jsonl"),
		claudeMsg(today.Add(time.Minute), "five", "claude-sonnet-5-5", million, 0, 0, 0))

	costs := func(rep statsReport) string {
		var out []string
		for _, r := range rep.Rows {
			out = append(out, r.Name+" "+money(r.Cost))
		}
		return strings.Join(out, ", ")
	}
	rep := statsOf(loadState(), readEvents(), 1, byTicket, now)
	if got := costs(rep); got != "FLD-1 $2.10, FLD-5 $2.00, FLD-2 $1.50" {
		t.Errorf("today's rows: %s", got)
	}
	if !near(rep.Total.Cost, 5.6) || rep.TicketsDone != 1 || rep.PrsMerged != 1 {
		t.Errorf("today: $%v, %d tickets done, %d PRs merged; want $5.60, 1 and 1", rep.Total.Cost, rep.TicketsDone, rep.PrsMerged)
	}
	if rep.Rows[0].Title != "One again" || rep.Rows[0].Repo != "app" {
		t.Errorf("FLD-1's row: %+v", rep.Rows[0])
	}
	// The key's two dispatches are one row; what the worktree held before the first isn't its.
	week := statsOf(loadState(), readEvents(), 7, byTicket, now)
	if got := costs(week); got != "FLD-1 $6.10, FLD-5 $2.00, FLD-2 $1.50" || week.TicketsDone != 2 {
		t.Errorf("the week's rows: %s, %d tickets done", got, week.TicketsDone)
	}
	if got := costs(statsOf(loadState(), readEvents(), 1, byRepo, now)); got != "app $3.60, other $2.00" {
		t.Errorf("by repo: %s", got)
	}

	var out bytes.Buffer
	if err := runStats([]string{"--by", "repo"}, &out); err != nil {
		t.Fatal(err)
	}
	if first := firstLine(out.String()); !strings.HasPrefix(first, "today · $5.60 · 2.1M tokens · ") || !strings.HasSuffix(first, "1 ticket done · 1 PR merged") {
		t.Errorf("foreman stats printed %q", out.String())
	}
	for _, bad := range [][]string{{"--days", "0"}, {"--by", "agent"}} {
		if err := runStats(bad, &out); err == nil {
			t.Errorf("foreman stats %v: want an error", bad)
		}
	}
}

func TestStatFigures(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{money(0), "$0.00"}, {money(0.004), "<$0.01"}, {money(12.396), "$12.40"},
		{tokenCount(950), "950"}, {tokenCount(81_240), "81.2k"}, {tokenCount(63_000_000), "63M"}, {tokenCount(999_600), "1M"},
		{workTime(0), "0m"}, {workTime(20), "<1m"}, {workTime(45 * 60), "45m"}, {workTime(3*3600 + 12*60), "3h 12m"},
		{periodName(1), "today"}, {periodName(7), "7 days"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	if p, ok := priceOf("claude-opus-5-5-20261101"); !ok || p.in != 4 {
		t.Errorf("a dated Opus 5.5: %+v, %t; want Opus 5.5's price, not Opus 5's", p, ok)
	}
}
