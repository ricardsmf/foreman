package main

// What an agent's status line shows of the queue. Claude Code runs the status line's command on
// every turn and every few seconds, so the command reads two files and calls nothing: the state,
// for the dispatch its pane belongs to, and the glance, which the board keeps as its queue changes.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
)

// A glance is the part of the queue a status line shows.
type glance struct {
	// Pid is the foreman whose queue wrote it. Once that one is gone nothing keeps the file, and
	// it is read as no glance at all.
	Pid int `json:"pid"`
	// Needs and Review are the items under needs you and under review, by dismissKey.
	Needs  []string `json:"needs,omitempty"`
	Review []string `json:"review,omitempty"`
	// Prs maps a dispatch to the open PR of mine on the branch its worktree is on.
	Prs map[string]glancePr `json:"prs,omitempty"`
}

type glancePr struct {
	Key    string `json:"key"`
	Number int    `json:"number"`
	// Says is what the PR waits on, or what someone else last did on it.
	Says  string `json:"says"`
	Group group  `json:"group"`
}

func glanceFile() string { return filepath.Join(filepath.Dir(stateFile), "glance.json") }

// glanceOf is the glance of the queue's items, as the foreman pid writes it.
func glanceOf(items []queueItem, prs []Pr, pid int) glance {
	g := glance{Pid: pid, Prs: map[string]glancePr{}}
	rows := map[string]queueItem{}
	for _, it := range items {
		if it.Kind == prItem {
			rows[it.Key] = it
		}
		switch it.Group {
		case needsYou:
			g.Needs = append(g.Needs, it.dismissKey())
		case toReview:
			g.Review = append(g.Review, it.dismissKey())
		}
	}
	for _, it := range items {
		if it.Kind != agentItem || it.Dir == "" {
			continue
		}
		if p, ok := prOn(prs, it.Dir); ok {
			row := rows[p.Key]
			g.Prs[it.Key] = glancePr{Key: p.Key, Number: p.Number, Says: cmp.Or(strings.TrimPrefix(row.News, "new: "), row.Wait), Group: row.Group}
		}
	}
	return g
}

var (
	glanceMu  sync.Mutex
	glanceSeq atomic.Int64
)

// glance writes the glance when the queue's changed. The latest alone is written: a write runs
// beside the next rebuild, and an older one landing last would stay until the queue changed again.
func (q *queueModel) glance() tea.Cmd {
	if q.glanced == nil {
		// An empty queue reads as no glance does, so one that starts empty writes nothing.
		q.glanced, _ = json.Marshal(glanceOf(nil, nil, os.Getpid()))
	}
	b, err := json.Marshal(glanceOf(q.all, q.prs, os.Getpid()))
	if err != nil || bytes.Equal(b, q.glanced) {
		return nil
	}
	q.glanced = b
	file, seq := glanceFile(), glanceSeq.Add(1)
	return func() tea.Msg {
		glanceMu.Lock()
		defer glanceMu.Unlock()
		if glanceSeq.Load() != seq {
			return nil
		}
		tmp := file + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, file)
		}
		return nil
	}
}

// readGlance is the glance a running foreman keeps, nil when none does.
func readGlance() *glance {
	b, err := os.ReadFile(glanceFile())
	if err != nil {
		return nil
	}
	var g glance
	if json.Unmarshal(b, &g) != nil || g.Pid == 0 || !askRuns(g.Pid) {
		return nil
	}
	return &g
}

// statusLine is the line: the dispatch, the PR on its branch with what that waits on, and how
// much else waits on me. Red and green mean what they do in the queue.
func statusLine(key string, g *glance) string {
	var parts []string
	if key != "" {
		parts = append(parts, key)
	}
	if g == nil {
		return strings.Join(parts, " · ")
	}
	own := []string{key}
	if p, ok := g.Prs[key]; ok && key != "" {
		own = append(own, prDismissKey(p.Key))
		text := fmt.Sprintf("#%d %s", p.Number, p.Says)
		switch p.Group {
		case needsYou:
			text = fg(cRed, text)
		case toReview:
			text = fg(cGreen, text)
		}
		parts = append(parts, text)
	}
	others := func(keys []string) int {
		n := 0
		for _, k := range keys {
			if !slices.Contains(own, k) {
				n++
			}
		}
		return n
	}
	var waits []string
	if n := others(g.Needs); n == 1 {
		waits = append(waits, fg(cRed, "1 needs you"))
	} else if n > 1 {
		waits = append(waits, fg(cRed, fmt.Sprintf("%d need you", n)))
	}
	if n := others(g.Review); n > 0 {
		waits = append(waits, fg(cGreen, fmt.Sprintf("%d to review", n)))
	}
	if len(waits) > 0 {
		parts = append(parts, "elsewhere "+strings.Join(waits, ", "))
	}
	return strings.Join(parts, " · ")
}

// runStatusline is `foreman statusline`, the command of a dispatched Claude Code's status line.
// stdin is what Claude Code hands such a command: JSON that names the directory it started in.
func runStatusline(stdin io.Reader, out io.Writer) error {
	var in struct {
		Cwd       string `json:"cwd"`
		Workspace struct {
			ProjectDir string `json:"project_dir"`
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	if b, err := io.ReadAll(stdin); err == nil {
		_ = json.Unmarshal(b, &in)
	}
	// Where it started, not where the agent has since moved to: the worktree is the dispatch's.
	dir := cmp.Or(in.Workspace.ProjectDir, in.Workspace.CurrentDir, in.Cwd)
	if dir == "" {
		dir, _ = os.Getwd()
	}
	// No dispatch is no error: the line then says only what waits on me.
	key, _ := askerKey(loadState(), os.Getenv("REX_BLOCK"), dir)
	if line := statusLine(key, readGlance()); line != "" {
		fmt.Fprintln(out, line)
	}
	return nil
}

// claudeSettings is what a dispatched Claude Code takes over my own settings: foreman's status
// line, run again every few seconds, since what waits elsewhere changes while the agent sits idle.
func claudeSettings() string {
	foreman := "'" + strings.ReplaceAll(which("foreman"), "'", `'\''`) + "'"
	b, _ := json.Marshal(map[string]any{"statusLine": map[string]any{
		"type": "command", "command": foreman + " statusline", "refreshInterval": statusLineEvery,
	}})
	return string(b)
}

// statusLineEvery is how often, in seconds, Claude Code runs the status line again by itself.
const statusLineEvery = 10
