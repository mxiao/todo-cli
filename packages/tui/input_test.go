package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKeys(t *testing.T) {
	cases := map[string]string{
		"a": "a", "G": "G", "?": "?", " ": "space", "\r": "enter", "\n": "enter", "\t": "tab",
		"\x7f": "backspace", "\x08": "backspace", "\x03": "ctrl+c", "\x10": "ctrl+p", "\x00": "ctrl+space",
		"\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left",
		"\x1bOA": "up", "\x1bOH": "home", "\x1b[H": "home", "\x1b[F": "end", "\x1b[1~": "home", "\x1b[4~": "end",
		"\x1b[3~": "delete", "\x1b[5~": "pgup", "\x1b[6~": "pgdown", "\x1bOP": "f1", "\x1b[11~": "f1", "\x1b[24~": "f12",
		"\x1b[Z": "shift+tab", "\x1b[1;2A": "shift+up", "\x1b[1;5C": "ctrl+right", "\x1b[1;3D": "alt+left",
		"\x1b[3;5~": "ctrl+delete", "\x1bx": "alt+x", "\x1b\x7f": "alt+backspace", "中": "中", "\x1c": "ctrl+\\",
	}
	for in, want := range cases {
		evs, n := ParseInput([]byte(in), false)
		if n != len(in) || len(evs) != 1 || evs[0].Kind != EvKey || evs[0].Key != want {
			t.Errorf("%q: got %+v (consumed %d), want key %q", in, evs, n, want)
		}
	}
	evs, _ := ParseInput([]byte("中"), false)
	if !evs[0].Printable() || evs[0].Rune != '中' {
		t.Errorf("CJK rune should be printable: %+v", evs[0])
	}
	if evs, _ := ParseInput([]byte("\x1bx"), false); evs[0].Printable() {
		t.Errorf("alt keys must not type text")
	}
}

func TestParseIncompleteAndEsc(t *testing.T) {
	for _, in := range []string{"\x1b", "\x1b[", "\x1b[1;", "\x1b[<0;10", "\x1b[M ", "\xe4\xb8", "\x1b[200~abc"} {
		evs, n := ParseInput([]byte(in), false)
		if len(evs) != 0 || n != 0 {
			t.Errorf("%q should wait for more input, got %+v n=%d", in, evs, n)
		}
	}
	evs, n := ParseInput([]byte("\x1b"), true)
	if n != 1 || len(evs) != 1 || evs[0].Key != "esc" {
		t.Errorf("lone ESC after timeout should be esc: %+v", evs)
	}
	evs, _ = ParseInput([]byte("j\x1b[Bk"), false)
	var keys []string
	for _, e := range evs {
		keys = append(keys, e.Key)
	}
	if strings.Join(keys, ",") != "j,down,k" {
		t.Errorf("sequence: %v", keys)
	}
	evs, _ = ParseInput([]byte("\x1b\x1b"), true)
	if len(evs) != 2 || evs[0].Key != "esc" || evs[1].Key != "esc" {
		t.Errorf("double esc: %+v", evs)
	}
}

func TestParseMouse(t *testing.T) {
	type m struct {
		x, y int
		b    MouseButton
		a    MouseAction
	}
	cases := map[string]m{
		"\x1b[<0;10;5M":   {9, 4, MouseLeft, MousePress},
		"\x1b[<0;10;5m":   {9, 4, MouseLeft, MouseRelease},
		"\x1b[<2;1;1M":    {0, 0, MouseRight, MousePress},
		"\x1b[<1;3;3M":    {2, 2, MouseMiddle, MousePress},
		"\x1b[<64;7;8M":   {6, 7, WheelUp, MousePress},
		"\x1b[<65;7;8M":   {6, 7, WheelDown, MousePress},
		"\x1b[<32;7;8M":   {6, 7, MouseLeft, MouseMotion},
		"\x1b[<4;120;40M": {119, 39, MouseLeft, MousePress}, // shift held
		"\x1b[M *%":       {9, 4, MouseLeft, MousePress},    // X10: 32+0, 33+9, 33+4
		"\x1b[M#*%":       {9, 4, MouseNone, MouseRelease},
		"\x1b[M`*%":       {9, 4, WheelUp, MousePress},
	}
	for in, want := range cases {
		evs, n := ParseInput([]byte(in), false)
		if n != len(in) || len(evs) != 1 || evs[0].Kind != EvMouse {
			t.Errorf("%q: %+v n=%d", in, evs, n)
			continue
		}
		e := evs[0]
		if (m{e.X, e.Y, e.Button, e.Action}) != want {
			t.Errorf("%q: got %+v want %+v", in, m{e.X, e.Y, e.Button, e.Action}, want)
		}
	}
}

func TestParsePaste(t *testing.T) {
	evs, n := ParseInput([]byte("\x1b[200~买牛奶 #home\x1b[201~x"), false)
	if n != len("\x1b[200~买牛奶 #home\x1b[201~x") || len(evs) != 2 || evs[0].Kind != EvPaste || evs[0].Text != "买牛奶 #home" || evs[1].Key != "x" {
		t.Errorf("paste: %+v", evs)
	}
}

func TestNormalizeKey(t *testing.T) {
	ok := map[string]string{
		"Ctrl+N": "ctrl+n", "ctrl+n": "ctrl+n", "alt+x": "alt+x", "option+x": "alt+x", "shift+tab": "shift+tab",
		"G": "G", "shift+g": "G", "?": "?", "+": "+", "-": "-", "F1": "f1", "PageDown": "pgdown", "Return": "enter",
		"Escape": "esc", " ": "space", "Space": "space", "ctrl+shift+up": "ctrl+shift+up", "中": "中", "ctrl++": "ctrl++",
	}
	for in, want := range ok {
		got, err := NormalizeKey(in)
		if err != nil || got != want {
			t.Errorf("NormalizeKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "hyper+x", "ctrl+i", "ctrl+m", "ctrl+h", "foo", "ctrl+foo"} {
		if _, err := NormalizeKey(bad); err == nil {
			t.Errorf("NormalizeKey(%q) should fail", bad)
		}
	}
}

func TestLoadKeymap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeymapFileName)
	km, err := LoadKeymap(path)
	if err != nil || km.Key(ActNew) != "a" || len(km.Custom) != 0 {
		t.Fatalf("missing file should give defaults: %v %v", err, km.Keys(ActNew))
	}
	os.WriteFile(path, []byte(`{"new": ["Ctrl+N", "a"], "delete": "D", "quit": "x", "archive": []}`), 0o600)
	km, err = LoadKeymap(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := km.Keys(ActNew); strings.Join(got, ",") != "ctrl+n,a" {
		t.Errorf("new keys: %v", got)
	}
	if act, _ := km.Lookup("D"); act != ActDelete {
		t.Errorf("D: %v", act)
	}
	if _, ok := km.Lookup("d"); ok {
		t.Errorf("d should no longer be bound (delete was redefined)")
	}
	// "x" moved from toggle_done to quit; toggle_done keeps space.
	if act, _ := km.Lookup("x"); act != ActQuit {
		t.Errorf("x: %v", act)
	}
	if strings.Join(km.Keys(ActToggleDone), ",") != "space" {
		t.Errorf("toggle_done keys: %v", km.Keys(ActToggleDone))
	}
	if len(km.Keys(ActArchive)) != 0 {
		t.Errorf("archive should be unbound")
	}
	if _, ok := km.Lookup("q"); ok {
		t.Errorf("q should be unbound after quit was redefined")
	}

	os.WriteFile(path, []byte(`{"nwe": "a", "edit": "hyper+e"}`), 0o600)
	km, err = LoadKeymap(path)
	if err == nil || !strings.Contains(err.Error(), `did you mean "new"`) || !strings.Contains(err.Error(), "hyper") {
		t.Errorf("bad config error: %v", err)
	}
	if km.Key(ActNew) != "a" || km.Path != path {
		t.Errorf("bad config should fall back to defaults")
	}
	os.WriteFile(path, []byte(`{not json`), 0o600)
	if _, err := LoadKeymap(path); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("invalid JSON: %v", err)
	}
	// The generated template round-trips to the defaults.
	os.WriteFile(path, DefaultKeymapJSON(), 0o600)
	km, err = LoadKeymap(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range DefaultKeymap().Bindings() {
		if strings.Join(km.Keys(b.Action), ",") != strings.Join(b.Keys, ",") {
			t.Errorf("template %s: %v vs %v", b.Action, km.Keys(b.Action), b.Keys)
		}
	}
}

func TestWidth(t *testing.T) {
	if textWidth("写周报abc") != 9 {
		t.Errorf("width: %d", textWidth("写周报abc"))
	}
	if got := truncate("写周报写周报", 7); got != "写周报…" || textWidth(got) != 7 {
		t.Errorf("truncate: %q", got)
	}
	if got := pad("中a", 5); got != "中a  " {
		t.Errorf("pad: %q", got)
	}
	if got := wrap("一二三四五", 4); strings.Join(got, "|") != "一二|三四|五" {
		t.Errorf("wrap: %v", got)
	}
	s := newScreen(6, 1)
	s.put(0, 0, "中文字", Style{}, 6)
	s.put(1, 0, "x", Style{}, 6) // overwrite right half of 中
	if got := s.Line(0); got != " x文字" {
		t.Errorf("overwriting half a wide char: %q", got)
	}
}
