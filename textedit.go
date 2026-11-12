package main

import (
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// A text field is its text and how many runes lie after its cursor. Counted from the end, the
// zero value is a cursor at the end, and text appended by other code leaves it there.

// editText applies an editing key to a field: moving by character, word or line, deleting the
// same, or typing. ok is false for a key that edits nothing. A caller with its own use for one of
// these keys handles it first.
func editText(text string, back int, msg tea.KeyPressMsg) (string, int, bool) {
	r := []rune(text)
	pos := len(r) - max(0, min(back, len(r)))
	lineStart, lineEnd := pos, pos
	for lineStart > 0 && r[lineStart-1] != '\n' {
		lineStart--
	}
	for lineEnd < len(r) && r[lineEnd] != '\n' {
		lineEnd++
	}
	// The start of the word before the cursor, and the end of the one after it.
	wordStart, wordEnd := pos, pos
	for wordStart > 0 && unicode.IsSpace(r[wordStart-1]) {
		wordStart--
	}
	for wordStart > 0 && !unicode.IsSpace(r[wordStart-1]) {
		wordStart--
	}
	for wordEnd < len(r) && unicode.IsSpace(r[wordEnd]) {
		wordEnd++
	}
	for wordEnd < len(r) && !unicode.IsSpace(r[wordEnd]) {
		wordEnd++
	}
	cut := func(from, to int) {
		r = append(r[:from:from], r[to:]...)
		pos = from
	}
	switch msg.String() {
	case "left", "ctrl+b":
		pos = max(0, pos-1)
	case "right", "ctrl+f":
		pos = min(len(r), pos+1)
	case "alt+left", "alt+b":
		pos = wordStart
	case "alt+right", "alt+f":
		pos = wordEnd
	case "home", "ctrl+a":
		pos = lineStart
	case "end", "ctrl+e":
		pos = lineEnd
	case "backspace":
		if pos > 0 {
			cut(pos-1, pos)
		}
	case "delete", "ctrl+d":
		if pos < len(r) {
			cut(pos, pos+1)
		}
	case "alt+backspace", "ctrl+w":
		cut(wordStart, pos)
	case "ctrl+u":
		cut(lineStart, pos)
	case "ctrl+k":
		cut(pos, lineEnd)
	default:
		typed := msg.Key().Text
		if typed == "" {
			return text, back, false
		}
		return insertText(text, back, typed), back, true
	}
	return string(r), len(r) - pos, true
}

// insertText puts s at the field's cursor, which stays after it.
func insertText(text string, back int, s string) string {
	r := []rune(text)
	pos := len(r) - max(0, min(back, len(r)))
	return string(r[:pos]) + s + string(r[pos:])
}

// cursorText is the field's text with its cursor drawn: a block at the end, else the character
// under it reversed.
func cursorText(text string, back int) string {
	r := []rune(text)
	if back <= 0 || back > len(r) {
		return text + fg(cMauve, "█")
	}
	pos := len(r) - back
	under := string(r[pos])
	if under == "\n" {
		// A line's end has no character to reverse: a space stands for it.
		under = " \x1b[27m\n\x1b[7m"
	}
	return string(r[:pos]) + "\x1b[7m" + under + "\x1b[27m" + string(r[pos+1:])
}
