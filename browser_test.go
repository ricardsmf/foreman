package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

const browserInTab = `{"self": {"tab": "window:1"}, "browsers": [
	{"key": "1-1", "inCurrentTab": false},
	{"key": "53782-1", "inCurrentTab": true, "pane": {"tab": "window:1"}, "tabs": []}]}`

// browsers is what the fakes of terminal-browser and the system browser were asked.
type browsers struct {
	calls  [][]string // terminal-browser's, the listing left out
	opened []string   // the system browser's
	tabErr error      // what new-tab fails with
}

// useFakeBrowsers puts the board in the Rex pane block, with a terminal-browser that lists ls.
func useFakeBrowsers(t *testing.T, block, ls string, lsErr error) *browsers {
	t.Helper()
	b := &browsers{}
	t.Setenv("REX_BLOCK", block)
	oldTB, oldOpen := terminalBrowser, openURL
	terminalBrowser = func(args ...string) (string, error) {
		if args[0] == "ls" {
			return ls, lsErr
		}
		b.calls = append(b.calls, args)
		return "", b.tabErr
	}
	openURL = func(u string) { b.opened = append(b.opened, u) }
	t.Cleanup(func() { terminalBrowser, openURL = oldTB, oldOpen })
	return b
}

// A page opens in a new tab of the browser already in this Rex tab, or in a browser split beside
// the board; outside Rex or without terminal-browser, in the system browser.
func TestBrowse(t *testing.T) {
	const url = "https://github.com/o/app/pull/5"
	for _, tc := range []struct {
		name, block, ls string
		lsErr           error
		want            string
	}{
		{"a browser in this tab", "b0", browserInTab, nil, "new-tab"},
		{"no browser yet", "b0", `{"browsers": []}`, nil, "split"},
		{"unreadable list", "b0", "terminal-browser: daemon starting", nil, "split"},
		{"no terminal-browser", "b0", "", exec.ErrNotFound, "open"},
		{"outside Rex", "", browserInTab, nil, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := useFakeRex(t)
			b := useFakeBrowsers(t, tc.block, tc.ls, tc.lsErr)
			if err := browse(url); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range f.calls {
				got = append(got, "rex "+c)
			}
			for _, c := range b.calls {
				got = append(got, "terminal-browser "+strings.Join(c, " "))
			}
			for _, u := range b.opened {
				got = append(got, "open "+u)
			}
			want := map[string]string{
				"new-tab": "terminal-browser new-tab --browser 53782-1 " + url,
				"split":   "rex split b0 " + strings.Join(browserPaneCmd(url), "\x00"),
				"open":    "open " + url,
			}[tc.want]
			if !slices.Equal(got, []string{want}) {
				t.Errorf("got %q; want only %q", got, want)
			}
		})
	}
}

// o opens the row's ticket or PR beside the board and O in the system browser, and either reads a
// PR's news; p opens a port beside the board.
func TestQueueOpensBesideTheBoard(t *testing.T) {
	tempState(t)
	useFakeRex(t)
	t.Setenv("REX_SESSION", "")
	b := useFakeBrowsers(t, "b0", browserInTab, nil)
	pr := Pr{Key: "app#10", Number: 10, Repo: "o/app", Summary: "fix: The gate", Branch: "rf/gate", Base: "develop",
		Mine: true, CI: "passed", Needs: "needs 1 approval, has 0", URL: "https://github.com/o/app/pull/10"}
	withNews := pr
	withNews.Key, withNews.Number, withNews.Branch, withNews.URL = "app#11", 11, "rf/other", "https://github.com/o/app/pull/11"
	withNews.News, withNews.NewsAt = "ana commented", time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	dispatched(t, "FLD-1", "/w/1")
	_, _ = updateState(func(s *State) { s.change("FLD-1", func(d *Dispatched) { d.Session = "s1" }) })
	q := &queueModel{details: map[string][]string{}, width: 100, height: 30,
		state:    loadState(),
		sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "idle", Ports: []rexPort{{Port: 3000, Addr: "127.0.0.1"}}}},
		prs:      []Pr{pr, withNews}}
	q.rebuild()
	// press sends the key on the row and runs what opens the page, the first of what it asks for.
	press := func(key, text string) {
		t.Helper()
		n := slices.IndexFunc(q.items, func(it queueItem) bool { return it.Key == key })
		if n < 0 {
			t.Fatalf("no row for %s", key)
		}
		q.cursor = n
		cmd := q.input(tea.KeyPressMsg{Code: rune(text[0]), Text: text})
		if cmd == nil {
			return
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			msg = batch[0]()
		}
		if msg != nil {
			t.Errorf("%s on %s: %+v", text, key, msg)
		}
	}
	tab := func(url string) []string { return []string{"new-tab", "--browser", "53782-1", url} }
	ticket := site + "/browse/FLD-1"

	press("FLD-1", "o")
	press("FLD-1", "p")
	press("app#10", "o")
	if want := [][]string{tab(ticket), tab("http://localhost:3000"), tab(pr.URL)}; !slices.EqualFunc(b.calls, want, slices.Equal[[]string]) || b.opened != nil {
		t.Errorf("terminal-browser calls %q, opened %q; want only %q", b.calls, b.opened, want)
	}
	if len(loadState().Dismissed) != 0 {
		t.Errorf("a PR with no news was marked reviewed: %v", loadState().Dismissed)
	}

	b.calls = nil
	press("FLD-1", "O")
	press("app#10", "O")
	if want := []string{ticket, pr.URL}; !slices.Equal(b.opened, want) || b.calls != nil {
		t.Errorf("O opened %q, terminal-browser calls %q; want only %q in the system browser", b.opened, b.calls, want)
	}

	for _, key := range []string{"o", "O"} {
		q.state, _ = updateState(func(s *State) { clear(s.Dismissed) })
		q.rebuild()
		b.calls, b.opened = nil, nil
		press("app#11", key)
		if len(b.calls)+len(b.opened) != 1 || len(loadState().Dismissed) != 1 {
			t.Errorf("%s on a PR with news: terminal-browser calls %q, opened %q, reviewed %v", key, b.calls, b.opened, loadState().Dismissed)
		}
	}

	// A tab that doesn't open is said.
	b.tabErr = errors.New("no browser 53782-1\nusage: terminal-browser new-tab")
	q.cursor = slices.IndexFunc(q.items, func(it queueItem) bool { return it.Key == "FLD-1" })
	msg, _ := q.input(tea.KeyPressMsg{Code: 'o', Text: "o"})().(queueStatusMsg)
	if msg.text != "Opening "+ticket+": no browser 53782-1" || msg.color != cRed {
		t.Errorf("status %+v", msg)
	}
}

// ^b opens the board's row beside the board: a ticket in Jira, a PR on GitHub.
func TestBoardOpensBesideTheBoard(t *testing.T) {
	tempState(t)
	useFakeRex(t)
	b := useFakeBrowsers(t, "b0", browserInTab, nil)
	m := boardOn(nil, Issue{Key: "FLD-7", Summary: "Fix it"})
	m.prFilter = "all"
	m.prs = []Row{{Key: "app#5", Summary: "A PR", Pr: &Pr{Key: "app#5", URL: "https://github.com/o/app/pull/5"}}}
	ctrlB := tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
	for _, want := range []string{site + "/browse/FLD-7", "https://github.com/o/app/pull/5"} {
		b.calls = nil
		cmd := m.boardInput(ctrlB)
		if cmd == nil {
			t.Fatalf("^b asked for nothing; want %s", want)
		}
		if msg := cmd(); msg != nil || len(b.calls) != 1 || !slices.Equal(b.calls[0], []string{"new-tab", "--browser", "53782-1", want}) || b.opened != nil {
			t.Errorf("terminal-browser calls %q, opened %q, said %+v; want a tab on %s", b.calls, b.opened, msg, want)
		}
		m.view = prs
	}
	b.tabErr = errors.New("no browser 53782-1")
	if msg, _ := m.boardInput(ctrlB)().(statusMsg); !strings.HasPrefix(msg.text, "Opening https://github.com/o/app/pull/5: no browser") {
		t.Errorf("status %+v", msg)
	}
}

// The wait says nothing when Rex sizes the pane at once, as it does when a client shows it, and
// says what it waits on once the wait passes two seconds.
func TestBrowserWaitScript(t *testing.T) {
	const wait = "Waiting for Rex to size this pane; ctrl+c closes it."
	for _, tc := range []struct {
		name   string
		checks int // how many size checks still find the placeholder
		want   []string
	}{
		{"sized at once", 3, []string{"terminal-browser open https://x.test --no-merge"}},
		{"sized late", 40, []string{wait, "terminal-browser open https://x.test --no-merge"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			fake := map[string]string{
				"stty":             "n=$(cat \"$FAKE_DIR/n\" 2>/dev/null || echo 0); n=$((n+1)); echo $n > \"$FAKE_DIR/n\"; if [ $n -le $FAKE_CHECKS ]; then echo 24 80; else echo 46 64; fi",
				"sleep":            ":",
				"terminal-browser": "echo terminal-browser \"$@\"",
			}
			for name, body := range fake {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			c := browserPaneCmd("https://x.test")
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DIR="+bin, "FAKE_CHECKS="+strconv.Itoa(tc.checks))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got := strings.Split(strings.TrimSpace(string(out)), "\n"); !slices.Equal(got, tc.want) {
				t.Errorf("printed %q; want %q", got, tc.want)
			}
		})
	}
}
