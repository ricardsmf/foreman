package main

import (
	"image/color"
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Each grey sits as far between the terminal's background and its text, in contrast, as
// Catppuccin's does on its dark theme: a soft theme's greys keep their steps below its text, and
// a light theme's read as well as a dark one's.
func TestGreysKeepTheirPlace(t *testing.T) {
	sub, over, sur, sel, pan := cSubtext, cOverlay, cSurface, bgSelected, bgPanel
	t.Cleanup(func() { cSubtext, cOverlay, cSurface, bgSelected, bgPanel = sub, over, sur, sel, pan })
	base := color.RGBA{0x24, 0x27, 0x3a, 0xff}
	top := math.Log(contrast(color.RGBA{0xca, 0xd3, 0xf5, 0xff}, base))
	names := []string{"subtext", "overlay", "surface", "selected", "panel"}
	var share []float64
	for _, c := range []color.Color{sub, over, sur, sel, pan} {
		share = append(share, math.Log(contrast(c, base))/top)
	}
	for _, theme := range []struct {
		name             string
		text, background color.Color
	}{
		{"Pierre Light", color.RGBA{0x52, 0x52, 0x52, 0xff}, color.White},
		{"Pierre Dark", color.RGBA{0xa3, 0xa3, 0xa3, 0xff}, color.RGBA{0x17, 0x17, 0x17, 0xff}},
		{"black on white", color.Black, color.White},
		{"Catppuccin Latte", color.RGBA{0x4c, 0x4f, 0x69, 0xff}, color.RGBA{0xef, 0xf1, 0xf5, 0xff}},
		{"Catppuccin Macchiato", color.RGBA{0xca, 0xd3, 0xf5, 0xff}, base},
	} {
		setTheme(theme.text, theme.background)
		greys := []color.Color{cSubtext, cOverlay, cSurface, bgSelected, bgPanel}
		for n, c := range greys {
			want := math.Pow(contrast(theme.text, theme.background), share[n])
			if got := contrast(c, theme.background); math.Abs(got-want)/want > 0.05 {
				t.Errorf("%s: %s %s is %.2f:1 against the background; want %.2f:1", theme.name, names[n], colourName(c), got, want)
			}
		}
		// The quiet text still reads, and the titles stay a step below the text.
		if got := contrast(cOverlay, theme.background); got < 3 {
			t.Errorf("%s: overlay %s is %.2f:1; want at least 3:1", theme.name, colourName(cOverlay), got)
		}
		if step := contrast(theme.text, theme.background) / contrast(cSubtext, theme.background); step < 1.25 {
			t.Errorf("%s: subtext %s is only %.2f times fainter than the text", theme.name, colourName(cSubtext), step)
		}
	}
}

// A group's heading is the list's structure: bold, in the text's colour unless it is blocked,
// with how many rows it holds.
func TestBoardHeadings(t *testing.T) {
	useFakeRex(t)
	tempState(t)
	tempHome(t)
	m := screenBoard(t)
	shown := strings.Split(looks(m.content()), "\n")
	heading := func(text string) string {
		for n, l := range shown {
			if strings.TrimSpace(l) == text {
				if n+1 < len(shown) && strings.HasPrefix(shown[n+1], "    ") {
					return strings.Join(strings.Fields(shown[n+1])[1:], " ")
				}
				return ""
			}
		}
		t.Errorf("no heading %q in:\n%s", text, ansi.Strip(m.content()))
		return ""
	}
	for text, want := range map[string]string{"In progress  2": "bold", "Blocked  1": "fg=ansi1 bold", "To do  1": "bold"} {
		if got := heading(text); !strings.HasPrefix(got, want) {
			t.Errorf("heading %q is %q; want %q", text, got, want)
		}
	}
}

// A rule parts the two sides, so the board and what sits beside it don't read as one wall.
func TestSidesRuled(t *testing.T) {
	a := testApp(t, false)
	bw, _ := a.widths()
	lines := strings.Split(a.content(), "\n")
	for n, l := range lines[:len(lines)-3] {
		cells := []rune(ansi.Strip(l))
		if len(cells) <= bw+1 || cells[bw+1] != '│' {
			t.Fatalf("line %d has no rule at column %d: %q", n, bw+1, ansi.Strip(l))
		}
	}
	if got := looks(ansi.Cut(lines[0], bw+1, bw+2)); !strings.Contains(got, "fg="+colourName(cSurface)) {
		t.Errorf("the rule is %q; want it in the surface grey", got)
	}
}
