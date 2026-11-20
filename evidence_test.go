package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The page shows videos, then images, then captures, each under its file name, escaped, and
// nothing else in the directory.
func TestEvidencePage(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"flow.mp4": "", "a-shot.png": "", `it's "quoted".png`: "", "screen.txt": "if a < b {",
		"report.md": "the report", "prompt.md": "the prompt", "notes.json": "{}", evidencePageName: "old page",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	page, err := evidencePage(dir, "FLD-1", "Fix <the> login")
	if err != nil {
		t.Fatal(err)
	}
	video, image, capture := strings.Index(page, `<video src="./flow.mp4" controls`), strings.Index(page, `<a href="./a-shot.png"><img src="./a-shot.png"`), strings.Index(page, "<pre>if a &lt; b {</pre>")
	if video < 0 || image < video || capture < image {
		t.Errorf("video at %d, image at %d, capture at %d:\n%s", video, image, capture, page)
	}
	for _, unwanted := range []string{"autoplay", `it's "quoted"`, "if a < b", "<the>", "the report", "the prompt", "notes.json", "old page", dir} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the page holds %q:\n%s", unwanted, page)
		}
	}
	for _, want := range []string{"<figcaption>it&#39;s &#34;quoted&#34;.png</figcaption>", "./it%27s%20%22quoted%22.png", "<figcaption>screen.txt</figcaption>", "Fix &lt;the&gt; login"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
}

// v shows the evidence page: in a new tab of the browser already in this Rex tab, or a browser
// split beside the board, or outside Rex or without terminal-browser, the media in their apps.
func TestQueueShowsEvidence(t *testing.T) {
	const inTab = `{"self": {"tab": "window:1"}, "browsers": [
		{"key": "1-1", "inCurrentTab": false},
		{"key": "53782-1", "inCurrentTab": true, "pane": {"tab": "window:1"}, "tabs": []}]}`
	for _, tc := range []struct {
		name, block, ls string
		lsErr           error
		files           []string
		want            string // the call that shows it, or nothing
		status          string
	}{
		{"no browser yet", "b0", `{"browsers": []}`, nil, []string{"a.png"}, "split", "beside the board"},
		{"unreadable list", "b0", "terminal-browser: daemon starting", nil, []string{"a.png"}, "split", "beside the board"},
		{"a browser in this tab", "b0", inTab, nil, []string{"a.png"}, "new-tab", "new tab"},
		{"no terminal-browser", "b0", "", exec.ErrNotFound, []string{"a.png"}, "open", ""},
		{"outside Rex", "", inTab, nil, []string{"a.png"}, "open", ""},
		{"captures alone, outside Rex", "", inTab, nil, []string{"screen.txt"}, "", "No screenshots or video"},
		{"nothing saved", "b0", inTab, nil, []string{"report.md"}, "", "No screenshots or video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempState(t)
			f := useFakeRex(t)
			t.Setenv("REX_SESSION", "")
			t.Setenv("REX_BLOCK", tc.block)
			var browserCalls [][]string
			var opened []string
			oldTB, oldOpen := terminalBrowser, openFiles
			terminalBrowser = func(args ...string) (string, error) {
				browserCalls = append(browserCalls, args)
				if args[0] == "ls" {
					return tc.ls, tc.lsErr
				}
				return "", nil
			}
			openFiles = func(paths ...string) error { opened = append(opened, paths...); return nil }
			t.Cleanup(func() { terminalBrowser, openFiles = oldTB, oldOpen })

			dir := evidenceDir("FLD-1")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			q := &queueModel{details: map[string][]string{}, width: 100, height: 30,
				state:    State{Dispatched: map[string]Dispatched{"FLD-1": {Dir: "/w/1", Session: "s1", Title: "Fix login"}}},
				sessions: []rexSession{{ID: "s1", Label: "fld-1", State: "idle"}}}
			q.rebuild()
			status := ""
			if cmd := q.input(tea.KeyPressMsg{Code: 'v', Text: "v"}); cmd != nil {
				if msg, ok := cmd().(queueStatusMsg); ok {
					status = msg.text
				}
			} else {
				status = q.status
			}
			if !strings.Contains(status, tc.status) {
				t.Errorf("status %q; want %q", status, tc.status)
			}

			page := filepath.Join(dir, evidencePageName)
			_, err := os.Stat(page)
			if written := err == nil; written != (tc.want == "split" || tc.want == "new-tab") {
				t.Errorf("the page written: %t", written)
			}
			split := slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "split ") })
			newTab := slices.ContainsFunc(browserCalls, func(c []string) bool { return c[0] == "new-tab" })
			switch tc.want {
			case "split":
				want := "split b0 " + strings.Join([]string{"sh", "-c", browserWaitScript, "sh", page}, "\x00")
				if !slices.Contains(f.calls, want) || newTab || opened != nil {
					t.Errorf("rex calls %q, browser calls %q, opened %q; want only %q", f.calls, browserCalls, opened, want)
				}
			case "new-tab":
				if want := []string{"new-tab", "--browser", "53782-1", page}; !slices.ContainsFunc(browserCalls, func(c []string) bool { return slices.Equal(c, want) }) || split || opened != nil {
					t.Errorf("rex calls %q, browser calls %q, opened %q; want only %q", f.calls, browserCalls, opened, want)
				}
			case "open":
				if want := []string{filepath.Join(dir, "a.png")}; !slices.Equal(opened, want) || split || newTab {
					t.Errorf("rex calls %q, browser calls %q, opened %q; want only %q", f.calls, browserCalls, opened, want)
				}
			default:
				if split || newTab || opened != nil {
					t.Errorf("rex calls %q, browser calls %q, opened %q; want none", f.calls, browserCalls, opened)
				}
			}
		})
	}
}

// The pane waits out Rex's placeholder size before it starts the browser on the page.
func TestBrowserPaneWaitsForItsSize(t *testing.T) {
	cmd := browserPaneCmd("/e/fld-1/.evidence.html")
	if cmd[0] != "sh" || cmd[len(cmd)-1] != "/e/fld-1/.evidence.html" {
		t.Errorf("command %q", cmd)
	}
	for _, want := range []string{`while [ "$(stty size)" = "24 80" ]; do sleep 0.1; done`, `exec terminal-browser open "$1" --no-merge`} {
		if !strings.Contains(cmd[2], want) {
			t.Errorf("the script lacks %q: %s", want, cmd[2])
		}
	}
	if tabBrowser("not json") != "" {
		t.Error("unreadable output named a browser")
	}
}
