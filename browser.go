package main

// Where a page opens: in terminal-browser beside the board when the board runs in Rex, else in
// the system browser.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
)

// openURL opens the URL in the system browser; tests put a fake here.
var openURL = func(url string) { _ = exec.Command("open", url).Start() }

// terminalBrowser runs terminal-browser and returns its stdout; tests put a fake here.
var terminalBrowser = func(args ...string) (string, error) { return sh("", "terminal-browser", args...) }

// browserWaitScript starts terminal-browser on $1 once Rex has sized the pane. Rex makes a pane at
// a placeholder 80x24 and sizes it seconds later; a browser started before that reads the jump in
// cell height as a font change and zooms the page by the ratio, a zoom Chromium then keeps for the
// URL. The wait has no limit: a pane still at the placeholder is one no client shows, and a browser
// started in it would be zoomed the moment one did. It says so only past two seconds, since a pane
// a client shows is sized within one.
const browserWaitScript = `i=0; while [ "$(stty size)" = "24 80" ]; do i=$((i+1)); if [ $i = 20 ]; then echo "Waiting for Rex to size this pane; ctrl+c closes it."; fi; sleep 0.1; done; exec terminal-browser open "$1" --no-merge`

// browserPaneCmd is the command of the pane that shows page.
func browserPaneCmd(page string) []string {
	return []string{"sh", "-c", browserWaitScript, "sh", page}
}

// tabBrowser is the key of a terminal-browser open in this Rex tab, from `terminal-browser ls
// --json`; output it can't read is no browser.
func tabBrowser(ls string) string {
	var out struct {
		Browsers []struct {
			Key          string `json:"key"`
			InCurrentTab bool   `json:"inCurrentTab"`
		} `json:"browsers"`
	}
	if json.Unmarshal([]byte(ls), &out) != nil {
		return ""
	}
	for _, b := range out.Browsers {
		if b.InCurrentTab && b.Key != "" {
			return b.Key
		}
	}
	return ""
}

// besideBoard is the board's Rex pane and the browsers terminal-browser lists. ok is false outside
// Rex or without terminal-browser, where nothing opens beside the board.
func besideBoard() (block, ls string, ok bool) {
	block = os.Getenv("REX_BLOCK")
	if block == "" {
		return "", "", false
	}
	ls, err := terminalBrowser("ls", "--json")
	return block, ls, !errors.Is(err, exec.ErrNotFound)
}

// showBeside opens target, a URL or a file, in a new tab of the browser already in this Rex tab,
// or else in a browser split right of the board's pane; newTab says which.
func showBeside(block, ls, target string) (newTab bool, err error) {
	if b := tabBrowser(ls); b != "" {
		_, err := terminalBrowser("new-tab", "--browser", b, target)
		return true, err
	}
	return false, rex.split(block, browserPaneCmd(target))
}

// browse opens the URL beside the board, or in the system browser where nothing opens beside it.
func browse(url string) error {
	block, ls, ok := besideBoard()
	if !ok {
		openURL(url)
		return nil
	}
	_, err := showBeside(block, ls, url)
	return err
}
