package tui

import (
	"fmt"
	"io"
	"strings"
)

// Style is a cell's visual attributes.
type Style struct {
	FG      int // 0 = default, otherwise an ANSI colour 1-15
	Bold    bool
	Dim     bool
	Reverse bool
	Under   bool
}

const (
	colRed     = 1
	colGreen   = 2
	colYellow  = 3
	colBlue    = 4
	colMagenta = 5
	colCyan    = 6
	colGray    = 8
)

type cell struct {
	r     rune // 0 marks the right half of a wide character
	style Style
}

// Screen is an off-screen frame of w×h cells.
type Screen struct {
	W, H  int
	cells [][]cell
	// CursorX/Y place the visible cursor when ShowCursor is set (inputs).
	CursorX, CursorY int
	ShowCursor       bool
}

func newScreen(w, h int) *Screen {
	s := &Screen{W: w, H: h, cells: make([][]cell, h)}
	for y := range s.cells {
		s.cells[y] = make([]cell, w)
		for x := range s.cells[y] {
			s.cells[y][x] = cell{r: ' '}
		}
	}
	return s
}

// put writes s at (x, y) clipped to maxX (exclusive) and returns the next x.
func (s *Screen) put(x, y int, text string, st Style, maxX int) int {
	if y < 0 || y >= s.H {
		return x
	}
	maxX = min(maxX, s.W)
	for _, r := range text {
		rw := runeWidth(r)
		if rw == 0 {
			continue
		}
		if x+rw > maxX {
			break
		}
		if x >= 0 {
			s.clearWide(x, y)
			if rw == 2 {
				s.clearWide(x+1, y)
			}
			s.cells[y][x] = cell{r: r, style: st}
			if rw == 2 {
				s.cells[y][x+1] = cell{r: 0, style: st}
			}
		}
		x += rw
	}
	return x
}

// clearWide blanks the other half of a wide character that (x, y) is about
// to overwrite, so no orphaned half is left behind.
func (s *Screen) clearWide(x, y int) {
	row := s.cells[y]
	if row[x].r == 0 && x > 0 {
		row[x-1] = cell{r: ' ', style: row[x-1].style}
	}
	if x+1 < len(row) && row[x+1].r == 0 {
		row[x+1] = cell{r: ' ', style: row[x+1].style}
	}
}

// fill paints a rectangle with spaces in style st.
func (s *Screen) fill(x, y, w, h int, st Style) {
	for yy := max(y, 0); yy < min(y+h, s.H); yy++ {
		for xx := max(x, 0); xx < min(x+w, s.W); xx++ {
			s.cells[yy][xx] = cell{r: ' ', style: st}
		}
	}
}

// box draws a bordered rectangle with an optional title and clears inside.
func (s *Screen) box(x, y, w, h int, title string, st Style) {
	if w < 2 || h < 2 {
		return
	}
	s.fill(x, y, w, h, Style{})
	s.put(x, y, "┌"+strings.Repeat("─", w-2)+"┐", st, x+w)
	for yy := y + 1; yy < y+h-1; yy++ {
		s.put(x, yy, "│", st, x+1)
		s.put(x+w-1, yy, "│", st, x+w)
	}
	s.put(x, y+h-1, "└"+strings.Repeat("─", w-2)+"┘", st, x+w)
	if title != "" {
		s.put(x+2, y, " "+title+" ", Style{Bold: true, FG: st.FG}, x+w-2)
	}
}

// Line returns row y as plain text (for tests and debugging).
func (s *Screen) Line(y int) string {
	var b strings.Builder
	for _, c := range s.cells[y] {
		if c.r != 0 {
			b.WriteRune(c.r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// String returns the whole frame as plain text.
func (s *Screen) String() string {
	lines := make([]string, s.H)
	for y := range lines {
		lines[y] = s.Line(y)
	}
	return strings.Join(lines, "\n")
}

// StyleAt returns the style of the cell at (x, y).
func (s *Screen) StyleAt(x, y int) Style { return s.cells[y][x].style }

func sgr(st Style, color bool) string {
	codes := []string{"0"}
	if st.Bold {
		codes = append(codes, "1")
	}
	if st.Dim {
		codes = append(codes, "2")
	}
	if st.Under {
		codes = append(codes, "4")
	}
	if st.Reverse {
		codes = append(codes, "7")
	}
	if color && st.FG > 0 {
		if st.FG < 8 {
			codes = append(codes, fmt.Sprint(30+st.FG))
		} else {
			codes = append(codes, fmt.Sprint(90+st.FG-8))
		}
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// renderLine encodes row y with SGR sequences.
func (s *Screen) renderLine(y int, color bool) string {
	var b strings.Builder
	cur := Style{FG: -1}
	for _, c := range s.cells[y] {
		if c.r == 0 {
			continue
		}
		if c.style != cur {
			b.WriteString(sgr(c.style, color))
			cur = c.style
		}
		b.WriteRune(c.r)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// flush writes the lines that differ from prev (all lines if prev is nil
// or a different size) and positions the cursor.
func (s *Screen) flush(w io.Writer, prev *Screen, color bool) error {
	var b strings.Builder
	full := prev == nil || prev.W != s.W || prev.H != s.H
	if full {
		b.WriteString("\x1b[0m\x1b[2J")
	}
	for y := 0; y < s.H; y++ {
		line := s.renderLine(y, color)
		if !full && line == prev.renderLine(y, color) {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s", y+1, line)
	}
	if s.ShowCursor {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", s.CursorY+1, s.CursorX+1)
	} else {
		b.WriteString("\x1b[?25l")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
