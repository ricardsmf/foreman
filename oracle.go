package main

// Oracles: the checks an agent names for its change. It records each result as it runs them,
// with foreman oracle, and the queue says how many are green and which were never seen red.

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

// An oracleRun is one result an agent recorded: a line of oracles.jsonl, beside its report.
type oracleRun struct {
	At     time.Time `json:"at"`
	Name   string    `json:"name"`
	Result string    `json:"result"` // red or green
	Note   string    `json:"note,omitempty"`
	// StaysGreen marks an oracle nothing can make red first, as the check a refactor must keep.
	StaysGreen bool `json:"staysGreen,omitempty"`
}

// An Oracle is one of a dispatch's oracles as it stands: its last result and note, and whether
// it was ever recorded red.
type Oracle struct {
	Name       string    `json:"name"`
	Result     string    `json:"result"`
	Note       string    `json:"note,omitempty"`
	At         time.Time `json:"at"`
	WasRed     bool      `json:"wasRed"`
	StaysGreen bool      `json:"staysGreen,omitempty"`
}

const (
	red   = "red"
	green = "green"
)

// neverRed is whether the oracle is green without having been red, when it could have been: it
// may have been written to fit the change, and then it checks nothing.
func (o Oracle) neverRed() bool { return o.Result == green && !o.WasRed && !o.StaysGreen }

func oraclesFile(key string) string { return filepath.Join(evidenceDir(key), "oracles.jsonl") }

func recordOracle(key string, run oracleRun) error {
	if err := os.MkdirAll(evidenceDir(key), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(oraclesFile(key), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// readOracles is the dispatch's oracles, in the order each was first recorded. A name is one
// oracle whatever its case.
func readOracles(key string) []Oracle {
	f, err := os.Open(oraclesFile(key))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []Oracle
	at := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var run oracleRun
		if json.Unmarshal(sc.Bytes(), &run) != nil || run.Name == "" {
			continue
		}
		name := strings.ToLower(run.Name)
		n, seen := at[name]
		if !seen {
			n = len(out)
			at[name] = n
			out = append(out, Oracle{Name: run.Name})
		}
		o := &out[n]
		o.Result, o.Note, o.At = run.Result, run.Note, run.At
		o.WasRed = o.WasRed || run.Result == red
		o.StaysGreen = o.StaysGreen || run.StaysGreen
	}
	return out
}

// oracleTally counts the oracles that are green, and those of them never seen red.
func oracleTally(oracles []Oracle) (greens, unseen int) {
	for _, o := range oracles {
		if o.Result == green {
			greens++
		}
		if o.neverRed() {
			unseen++
		}
	}
	return greens, unseen
}

// oracleCount is the oracles in a few words, 3/5 green, with those never red; "" without any.
func oracleCount(oracles []Oracle) string {
	if len(oracles) == 0 {
		return ""
	}
	greens, unseen := oracleTally(oracles)
	count := fmt.Sprintf("%d/%d green", greens, len(oracles))
	if unseen > 0 {
		count += fmt.Sprintf(", %d never red", unseen)
	}
	return count
}

// oracleMark is that count for a queue row, green once every oracle is and each was seen red.
func oracleMark(oracles []Oracle) string {
	if len(oracles) == 0 {
		return ""
	}
	if greens, unseen := oracleTally(oracles); greens < len(oracles) || unseen > 0 {
		return fg(cYellow, oracleCount(oracles))
	}
	return fg(cGreen, oracleCount(oracles))
}

// oracleLines list the oracles for the queue's detail pane.
func oracleLines(oracles []Oracle) []string {
	if len(oracles) == 0 {
		return nil
	}
	out := []string{"## Oracles", "", oracleCount(oracles), ""}
	for _, o := range oracles {
		line := "- ✓ `" + o.Name + "` green"
		switch {
		case o.Result != green:
			line = "- ✗ `" + o.Name + "` **" + o.Result + "**"
		case o.StaysGreen && !o.WasRed:
			line += ", stays green"
		case !o.WasRed:
			line += ", **never red**"
		default:
			line += ", was red"
		}
		if o.Note != "" {
			line += ": " + strings.Join(strings.Fields(o.Note), " ")
		}
		out = append(out, line)
	}
	return out
}

// oracleSection tells an implementing agent to record each oracle's result as it runs it.
func oracleSection() []string {
	return []string{
		"",
		"## Oracles",
		"",
		"Record each oracle's result when you run it, under one name: `foreman oracle \"<name>\" red \"<what it showed>\"` before the change, and `foreman oracle \"<name>\" green \"<what it showed>\"` once it passes. My queue counts them, and marks one that was never red.",
		"For an oracle that nothing can make red first, add `--stays-green`.",
	}
}

func runOracle(argv []string, out io.Writer) error {
	fs := newFlags("oracle")
	key := fs.String("key", "", "")
	stays := fs.Bool("stays-green", false, "")
	words, err := parseFlags(fs, argv, -1)
	if err != nil {
		return err
	}
	if len(words) < 2 {
		return usageError(errors.New("oracle takes the oracle's name and its result, red or green"))
	}
	name, result := strings.TrimSpace(words[0]), strings.ToLower(words[1])
	switch {
	case name == "":
		return usageError(errors.New("the oracle needs a name"))
	case result != red && result != green:
		return usageError(fmt.Errorf("an oracle is red or green, not %q; quote a name of several words", words[1]))
	case *stays && result == red:
		return usageError(errors.New("--stays-green is for an oracle that is green"))
	}
	if *key == "" {
		cwd, _ := os.Getwd()
		if *key, err = askerKey(loadState(), os.Getenv("REX_BLOCK"), cwd); err != nil {
			return err
		}
	}
	run := oracleRun{At: time.Now(), Name: name, Result: result, Note: strings.Join(words[2:], " "), StaysGreen: *stays}
	if err := recordOracle(*key, run); err != nil {
		return err
	}
	oracles := readOracles(*key)
	fmt.Fprintf(out, "%s: %s\n", *key, oracleCount(oracles))
	for _, o := range oracles {
		if strings.EqualFold(o.Name, name) && o.neverRed() {
			fmt.Fprintf(out, "%q is green and was never recorded red: an oracle that could not fail before the change checks nothing. Where nothing can make it red first, record it with --stays-green.\n", o.Name)
		}
	}
	return nil
}
