package tui

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EventKind tells which fields of an Event are meaningful.
type EventKind int

const (
	EvKey    EventKind = iota + 1 // Key (and Rune for printable keys)
	EvMouse                       // X, Y, Button, Action
	EvPaste                       // Text
	EvResize                      // W, H
	EvTick                        // periodic refresh check
)

// MouseButton identifies the button in a mouse event.
type MouseButton int

const (
	MouseNone MouseButton = iota
	MouseLeft
	MouseMiddle
	MouseRight
	WheelUp
	WheelDown
)

// MouseAction distinguishes presses from releases and drags.
type MouseAction int

const (
	MousePress MouseAction = iota
	MouseRelease
	MouseMotion
)

// Event is one decoded terminal input event.
type Event struct {
	Kind EventKind
	// Key is the canonical key name: a printable character ("a", "A", "?",
	// "中"), "space", "enter", "tab", "esc", "backspace", "up", "f1", ...,
	// optionally prefixed with "ctrl+", "alt+" and/or "shift+".
	Key  string
	Rune rune // printable character for typed text; 0 otherwise

	X, Y   int // 0-based cell coordinates of a mouse event
	Button MouseButton
	Action MouseAction

	Text string // bracketed paste payload
	W, H int    // resize
}

// Printable reports whether the event types a character into an input.
func (e Event) Printable() bool { return e.Kind == EvKey && e.Rune != 0 }

func keyEv(name string) Event { return Event{Kind: EvKey, Key: name} }

func runeEv(r rune, alt bool) Event {
	name := string(r)
	if r == ' ' {
		name = "space"
	}
	if alt {
		return Event{Kind: EvKey, Key: "alt+" + name}
	}
	return Event{Kind: EvKey, Key: name, Rune: r}
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// ParseInput decodes as many complete events as possible from b and returns
// them with the number of bytes consumed. A trailing incomplete escape
// sequence is left unconsumed unless final is set, in which case a lone ESC
// becomes the esc key (the caller uses final after a short read timeout).
func ParseInput(b []byte, final bool) ([]Event, int) {
	var out []Event
	i := 0
	for i < len(b) {
		ev, n := parseOne(b[i:])
		if n == 0 { // incomplete
			if !final {
				break
			}
			if b[i] == 0x1b {
				ev, n = keyEv("esc"), 1
			} else {
				i = len(b) // invalid partial UTF-8: drop
				break
			}
		}
		if ev.Kind != 0 {
			out = append(out, ev)
		}
		i += n
	}
	return out, i
}

// parseOne decodes one event at the start of b. n == 0 means b holds an
// incomplete sequence; ev.Kind == 0 with n > 0 means bytes to ignore.
func parseOne(b []byte) (Event, int) {
	c := b[0]
	switch {
	case c == 0x1b:
		return parseEscape(b)
	case c == '\r' || c == '\n':
		return keyEv("enter"), 1
	case c == '\t':
		return keyEv("tab"), 1
	case c == 0x7f || c == 0x08:
		return keyEv("backspace"), 1
	case c == 0:
		return keyEv("ctrl+space"), 1
	case c <= 26:
		return keyEv("ctrl+" + string(rune('a'+c-1))), 1
	case c < 0x20:
		return keyEv("ctrl+" + string("\\]^_"[c-0x1c])), 1
	case c < 0x80:
		return runeEv(rune(c), false), 1
	}
	if !utf8.FullRune(b) {
		return Event{}, 0
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError {
		return Event{}, n
	}
	return runeEv(r, false), n
}

func parseEscape(b []byte) (Event, int) {
	if len(b) == 1 {
		return Event{}, 0
	}
	switch b[1] {
	case '[':
		return parseCSI(b)
	case 'O':
		if len(b) < 3 {
			return Event{}, 0
		}
		if name, ok := ss3Keys[b[2]]; ok {
			return keyEv(name), 3
		}
		return Event{}, 3
	case 0x1b:
		// ESC ESC: an esc keypress followed by something else.
		return keyEv("esc"), 1
	}
	// Alt/Option as Meta: ESC followed by a key.
	ev, n := parseOne(b[1:])
	if n == 0 {
		return Event{}, 0
	}
	if ev.Kind == EvKey && !strings.HasPrefix(ev.Key, "alt+") {
		ev.Key, ev.Rune = "alt+"+ev.Key, 0
	}
	return ev, n + 1
}

var ss3Keys = map[byte]string{
	'A': "up", 'B': "down", 'C': "right", 'D': "left", 'H': "home", 'F': "end",
	'P': "f1", 'Q': "f2", 'R': "f3", 'S': "f4", 'M': "enter",
}

var csiLetterKeys = map[byte]string{
	'A': "up", 'B': "down", 'C': "right", 'D': "left", 'H': "home", 'F': "end",
	'P': "f1", 'Q': "f2", 'R': "f3", 'S': "f4",
}

var csiTildeKeys = map[int]string{
	1: "home", 2: "insert", 3: "delete", 4: "end", 5: "pgup", 6: "pgdown", 7: "home", 8: "end",
	11: "f1", 12: "f2", 13: "f3", 14: "f4", 15: "f5", 17: "f6", 18: "f7", 19: "f8",
	20: "f9", 21: "f10", 23: "f11", 24: "f12",
}

func parseCSI(b []byte) (Event, int) {
	if len(b) < 3 {
		return Event{}, 0
	}
	if b[2] == 'M' { // X10 mouse: ESC [ M Cb Cx Cy
		if len(b) < 6 {
			return Event{}, 0
		}
		return mouseEvent(int(b[3])-32, int(b[4])-33, int(b[5])-33, false), 6
	}
	if bytes.HasPrefix(b, []byte(pasteStart)) {
		end := bytes.Index(b, []byte(pasteEnd))
		if end < 0 {
			return Event{}, 0
		}
		text := string(b[len(pasteStart):end])
		return Event{Kind: EvPaste, Text: text}, end + len(pasteEnd)
	}
	// Parameter bytes 0x30-0x3F, intermediates 0x20-0x2F, final 0x40-0x7E.
	j := 2
	for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
		j++
	}
	if j >= len(b) {
		if len(b) > 64 { // runaway sequence: discard
			return Event{}, len(b)
		}
		return Event{}, 0
	}
	final := b[j]
	params := string(b[2:j])
	n := j + 1
	if strings.HasPrefix(params, "<") && (final == 'M' || final == 'm') { // SGR mouse
		p := splitInts(params[1:])
		if len(p) != 3 {
			return Event{}, n
		}
		return mouseEvent(p[0], p[1]-1, p[2]-1, final == 'm'), n
	}
	p := splitInts(params)
	mod := ""
	if len(p) >= 2 {
		mod = modifierPrefix(p[1])
	}
	switch {
	case final == '~' && len(p) >= 1:
		if name, ok := csiTildeKeys[p[0]]; ok {
			return keyEv(mod + name), n
		}
	case final == 'Z':
		return keyEv("shift+tab"), n
	case final == 'I' || final == 'O': // focus in/out
		return Event{}, n
	default:
		if name, ok := csiLetterKeys[final]; ok {
			return keyEv(mod + name), n
		}
	}
	return Event{}, n
}

func splitInts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ";") {
		v, err := strconv.Atoi(f)
		if err != nil {
			v = 0
		}
		out = append(out, v)
	}
	return out
}

// modifierPrefix decodes the xterm modifier parameter (1 + bitmask).
func modifierPrefix(m int) string {
	if m < 2 {
		return ""
	}
	m--
	var s string
	if m&4 != 0 {
		s += "ctrl+"
	}
	if m&2 != 0 {
		s += "alt+"
	}
	if m&1 != 0 {
		s += "shift+"
	}
	return s
}

func mouseEvent(cb, x, y int, release bool) Event {
	ev := Event{Kind: EvMouse, X: max(x, 0), Y: max(y, 0)}
	switch {
	case cb&64 != 0:
		switch cb & 3 {
		case 0:
			ev.Button = WheelUp
		case 1:
			ev.Button = WheelDown
		default:
			return Event{} // horizontal wheel: ignored
		}
	default:
		switch cb & 3 {
		case 0:
			ev.Button = MouseLeft
		case 1:
			ev.Button = MouseMiddle
		case 2:
			ev.Button = MouseRight
		case 3:
			ev.Button = MouseNone
			release = true // X10 release carries no button
		}
	}
	switch {
	case release:
		ev.Action = MouseRelease
	case cb&32 != 0:
		ev.Action = MouseMotion
	}
	return ev
}
