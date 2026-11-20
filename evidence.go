package main

// The evidence page: what an agent saved, in one page that `v` opens beside the board.

import (
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
	block, ls, ok := besideBoard()
	if !ok {
		return fallback()
	}
	html, err := evidencePage(dir, key, title)
	if err == nil {
		err = os.WriteFile(page, []byte(html), 0o644)
	}
	if err != nil {
		return queueStatusMsg{text: "Writing " + key + "'s evidence page: " + err.Error(), color: cRed}
	}
	newTab, err := showBeside(block, ls, page)
	if err != nil {
		return queueStatusMsg{text: "Opening " + key + "'s evidence: " + firstLine(err.Error()), color: cRed}
	}
	if newTab {
		return queueStatusMsg{text: "✓ Opened " + key + "'s evidence in a new tab of the browser beside", color: cGreen}
	}
	return queueStatusMsg{text: "✓ Opened " + key + "'s evidence beside the board", color: cGreen}
}
