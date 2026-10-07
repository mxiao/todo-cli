package tui

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"
)

// ErrNotTerminal is returned when stdin/stdout are not an interactive terminal.
var ErrNotTerminal = errors.New("not_a_terminal")

const (
	enterAltScreen = "\x1b[?1049h\x1b[H\x1b[2J"
	leaveAltScreen = "\x1b[?1049l"
	hideCursor     = "\x1b[?25l"
	showCursor     = "\x1b[?25h"
	// Button press/release tracking with SGR (1006) coordinates; terminals
	// without mouse support ignore these private modes.
	mouseOn        = "\x1b[?1000h\x1b[?1006h"
	mouseOff       = "\x1b[?1006l\x1b[?1000l"
	pasteOn        = "\x1b[?2004h"
	pasteOff       = "\x1b[?2004l"
	escTimeout     = 30 * time.Millisecond
	refreshEvery   = time.Second
	resetAttribute = "\x1b[0m"
)

// Run takes over the terminal on in/out until the user quits. The terminal
// state is restored on every exit path, including panics.
func Run(o Options, in, out *os.File) (err error) {
	inFd, outFd := int(in.Fd()), int(out.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(outFd) {
		return fmt.Errorf("%w: the interactive UI needs a terminal on stdin and stdout; use the non-interactive commands (todo help) in scripts", ErrNotTerminal)
	}
	if t := os.Getenv("TERM"); t == "dumb" {
		return fmt.Errorf("%w: TERM=dumb does not support the interactive UI; use the non-interactive commands (todo help)", ErrNotTerminal)
	}
	w, h, err := term.GetSize(outFd)
	if err != nil || w <= 0 || h <= 0 {
		w, h = 80, 24
	}
	app, err := New(o, w, h)
	if err != nil {
		return err
	}
	state, err := term.MakeRaw(inFd)
	if err != nil {
		return fmt.Errorf("enter raw mode: %w", err)
	}
	bw := bufio.NewWriterSize(out, 64<<10)
	setup := enterAltScreen + hideCursor + pasteOn
	teardown := resetAttribute + pasteOff + showCursor + leaveAltScreen
	if o.Mouse {
		setup += mouseOn
		teardown = mouseOff + teardown
	}
	restore := func() {
		bw.WriteString(teardown)
		bw.Flush()
		term.Restore(inFd, state)
	}
	defer func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
		restore()
	}()
	bw.WriteString(setup)

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)

	input := make(chan []byte, 16)
	go readInput(in, input)
	ticker := time.NewTicker(refreshEvery)
	defer ticker.Stop()

	var prev *Screen
	var pending []byte
	var escTimer <-chan time.Time
	for !app.Quit() {
		frame := app.Render()
		if err := frame.flush(bw, prev, o.Color); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		prev = frame
		select {
		case b, ok := <-input:
			if !ok {
				return nil // stdin closed
			}
			pending = append(pending, b...)
			evs, n := ParseInput(pending, false)
			pending = pending[n:]
			for _, ev := range evs {
				app.HandleEvent(ev)
			}
			escTimer = nil
			if len(pending) > 0 {
				escTimer = time.After(escTimeout)
			}
		case <-escTimer:
			evs, n := ParseInput(pending, true)
			pending = pending[n:]
			for _, ev := range evs {
				app.HandleEvent(ev)
			}
			escTimer = nil
		case s := <-sig:
			if s != syscall.SIGWINCH {
				return nil
			}
			if nw, nh, err := term.GetSize(outFd); err == nil {
				app.HandleEvent(Event{Kind: EvResize, W: nw, H: nh})
				prev = nil
			}
		case <-ticker.C:
			app.HandleEvent(Event{Kind: EvTick})
		}
	}
	return nil
}

func readInput(f *os.File, ch chan<- []byte) {
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			ch <- append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			close(ch)
			return
		}
	}
}
