package tui

import (
	"strings"
	"unicode"
)

// lineEditor is a single-line text input with emacs-style editing keys.
type lineEditor struct {
	text []rune
	pos  int
}

func newLineEditor(s string) *lineEditor {
	r := []rune(s)
	return &lineEditor{text: r, pos: len(r)}
}

func (e *lineEditor) String() string { return string(e.text) }

func (e *lineEditor) set(s string) {
	e.text = []rune(s)
	e.pos = len(e.text)
}

func (e *lineEditor) insert(s string) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	r := []rune(s)
	e.text = append(e.text[:e.pos], append(r, e.text[e.pos:]...)...)
	e.pos += len(r)
}

// handle applies an editing key and reports whether it was consumed.
func (e *lineEditor) handle(ev Event) bool {
	if ev.Kind == EvPaste {
		e.insert(ev.Text)
		return true
	}
	if ev.Printable() {
		e.insert(string(ev.Rune))
		return true
	}
	switch ev.Key {
	case "space":
		e.insert(" ")
	case "left", "ctrl+b":
		e.pos = max(e.pos-1, 0)
	case "right", "ctrl+f":
		e.pos = min(e.pos+1, len(e.text))
	case "home", "ctrl+a":
		e.pos = 0
	case "end", "ctrl+e":
		e.pos = len(e.text)
	case "backspace":
		if e.pos > 0 {
			e.text = append(e.text[:e.pos-1], e.text[e.pos:]...)
			e.pos--
		}
	case "delete", "ctrl+d":
		if e.pos < len(e.text) {
			e.text = append(e.text[:e.pos], e.text[e.pos+1:]...)
		}
	case "ctrl+u":
		e.text = e.text[e.pos:]
		e.pos = 0
	case "ctrl+k":
		e.text = e.text[:e.pos]
	case "ctrl+w", "alt+backspace":
		i := e.pos
		for i > 0 && e.text[i-1] == ' ' {
			i--
		}
		for i > 0 && e.text[i-1] != ' ' {
			i--
		}
		e.text = append(e.text[:i], e.text[e.pos:]...)
		e.pos = i
	case "alt+b", "ctrl+left", "alt+left":
		for e.pos > 0 && e.text[e.pos-1] == ' ' {
			e.pos--
		}
		for e.pos > 0 && e.text[e.pos-1] != ' ' {
			e.pos--
		}
	case "alt+f", "ctrl+right", "alt+right":
		for e.pos < len(e.text) && e.text[e.pos] == ' ' {
			e.pos++
		}
		for e.pos < len(e.text) && e.text[e.pos] != ' ' {
			e.pos++
		}
	default:
		return false
	}
	return true
}

// lastToken returns the word being typed at the end of the input.
func (e *lineEditor) lastToken() string {
	s := string(e.text[:e.pos])
	if i := strings.LastIndexAny(s, " ,"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// completeToken replaces the word at the cursor with word.
func (e *lineEditor) completeToken(word string) {
	tok := []rune(e.lastToken())
	start := e.pos - len(tok)
	rest := e.text[e.pos:]
	e.text = append(append(append([]rune{}, e.text[:start]...), []rune(word)...), rest...)
	e.pos = start + len([]rune(word))
}

// render returns the visible text and cursor column for a field w columns
// wide, scrolling horizontally so the cursor stays visible.
func (e *lineEditor) render(w int) (string, int) {
	if w <= 1 {
		return "", 0
	}
	start := 0
	for textWidth(string(e.text[start:e.pos])) > w-1 {
		start++
	}
	vis := []rune{}
	used := 0
	for _, r := range e.text[start:] {
		rw := runeWidth(r)
		if used+rw > w {
			break
		}
		vis = append(vis, r)
		used += rw
	}
	return string(vis), textWidth(string(e.text[start:e.pos]))
}
