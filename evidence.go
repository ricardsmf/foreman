package main

// The evidence page: what an agent saved, in one page that `v` opens in a terminal-browser pane
// beside the board.

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// evidencePageName is the page in the evidence directory: hidden, so evidence() doesn't count it
// and no agent writes it.
const evidencePageName = ".evidence.html"

// terminalBrowser runs terminal-browser and returns its stdout; tests put a fake here.
var terminalBrowser = func(args ...string) (string, error) { return sh("", "terminal-browser", args...) }

// openFiles opens files in their default apps; tests put a fake here.
var openFiles = func(paths ...string) error { return exec.Command("open", paths...).Start() }

type evidenceFile struct{ Name, Href, Text string }

var evidenceTmpl = template.Must(template.New("evidence").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Key}}{{with .Title}}: {{.}}{{end}}</title>
<style>
  * { box-sizing: border-box; }
  body { margin: 0 auto; max-width: 72rem; padding: 1rem; background: #1e1e2e; color: #cdd6f4;
    font: 15px/1.45 -apple-system, BlinkMacSystemFont, system-ui, sans-serif; }
  h1 { font-size: 1.2rem; margin: 0 0 1rem; }
  figure { margin: 0 0 1.5rem; min-width: 0; }
  figcaption { color: #a6adc8; font-size: 0.85rem; margin: 0.3rem 0; overflow-wrap: anywhere; }
  video, img { display: block; max-width: 100%; height: auto; border-radius: 4px; }
  a { color: #89b4fa; }
  pre { margin: 0; padding: 0.75rem; background: #181825; border-radius: 4px; overflow-x: auto;
    white-space: pre-wrap; overflow-wrap: anywhere; font: 13px/1.35 ui-monospace, Menlo, monospace; }
</style>
</head>
<body>
<h1>{{.Key}}{{with .Title}}: {{.}}{{end}}</h1>
{{range .Videos}}<figure><figcaption>{{.Name}}</figcaption><video src="{{.Href}}" controls preload="metadata"></video></figure>
{{end}}{{range .Images}}<figure><figcaption>{{.Name}}</figcaption><a href="{{.Href}}"><img src="{{.Href}}" alt="{{.Name}}"></a></figure>
{{end}}{{range .Captures}}<figure><figcaption>{{.Name}}</figcaption><pre>{{.Text}}</pre></figure>
{{end}}</body>
</html>
`))

// evidencePage is the page for what evidence() finds in dir: videos, then images, then terminal
// captures inlined, each under its file name. Links are relative to dir, where the page goes.
func evidencePage(dir, key, title string) (string, error) {
	media, captures := evidence(dir)
	var data struct {
		Key, Title               string
		Videos, Images, Captures []evidenceFile
	}
	data.Key, data.Title = key, title
	for _, p := range media {
		f := evidenceFile{Name: filepath.Base(p), Href: "./" + url.PathEscape(filepath.Base(p))}
		if slices.Contains(videoExt, strings.ToLower(filepath.Ext(p))) {
			data.Videos = append(data.Videos, f)
		} else {
			data.Images = append(data.Images, f)
		}
	}
	for _, p := range captures {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		data.Captures = append(data.Captures, evidenceFile{Name: filepath.Base(p), Text: string(b)})
	}
	var out strings.Builder
	err := evidenceTmpl.Execute(&out, data)
	return out.String(), err
}

// browserWaitScript starts terminal-browser on $1 once Rex has sized the pane. Rex makes a pane at
// a placeholder 80x24 and sizes it seconds later; a browser started before that reads the jump in
// cell height as a font change and zooms the page by the ratio, a zoom Chromium then keeps for the
// URL. The wait has no limit: a pane still at the placeholder is one no client shows, and a browser
// started in it would be zoomed the moment one did.
const browserWaitScript = `echo "Waiting for Rex to size this pane; ctrl+c closes it."; while [ "$(stty size)" = "24 80" ]; do sleep 0.1; done; exec terminal-browser open "$1" --no-merge`

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

// showEvidence writes the key's evidence page and opens it: in a new tab of the browser already in
// this Rex tab, or else in a browser split right of the board's pane. Outside Rex, or without
// terminal-browser, the media open in their default apps.
func showEvidence(key, title string, media []string) tea.Msg {
	dir := evidenceDir(key)
	page := filepath.Join(dir, evidencePageName)
	fallback := func() tea.Msg {
		if len(media) == 0 {
			return queueStatusMsg{text: "No screenshots or video for " + key + ", and no terminal-browser to show its captures", color: cOverlay}
		}
		if err := openFiles(media...); err != nil {
			return queueStatusMsg{text: "Opening " + key + "'s evidence: " + err.Error(), color: cRed}
		}
		return nil
	}
	block := os.Getenv("REX_BLOCK")
	if block == "" {
		return fallback()
	}
	ls, err := terminalBrowser("ls", "--json")
	if errors.Is(err, exec.ErrNotFound) {
		return fallback()
	}
	html, err := evidencePage(dir, key, title)
	if err == nil {
		err = os.WriteFile(page, []byte(html), 0o644)
	}
	if err != nil {
		return queueStatusMsg{text: "Writing " + key + "'s evidence page: " + err.Error(), color: cRed}
	}
	if b := tabBrowser(ls); b != "" {
		if _, err := terminalBrowser("new-tab", "--browser", b, page); err != nil {
			return queueStatusMsg{text: "Opening " + key + "'s evidence: " + firstLine(err.Error()), color: cRed}
		}
		return queueStatusMsg{text: "✓ Opened " + key + "'s evidence in a new tab of the browser beside", color: cGreen}
	}
	if err := rex.split(block, browserPaneCmd(page)); err != nil {
		return queueStatusMsg{text: "Opening " + key + "'s evidence: " + firstLine(err.Error()), color: cRed}
	}
	return queueStatusMsg{text: "✓ Opened " + key + "'s evidence beside the board", color: cGreen}
}
