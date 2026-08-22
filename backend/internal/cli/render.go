package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Drawing vocabulary. Block elements and braille render on every terminal
// worth animating on; the plain fallback below covers the rest.
const (
	fillRune  = "━"
	trackRune = "─"
)

// spinnerFrames is a braille wheel: eight frames, one cell wide.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧"}

// ANSI control sequences. Only the handful needed to repaint a block of
// lines in place, so no terminfo database is required.
const (
	ansiHideCursor = "\x1b[?25l"
	ansiShowCursor = "\x1b[?25h"
	ansiClearLine  = "\x1b[2K"
	ansiLineStart  = "\r"
)

// colour is an SGR pair; empty when the terminal is not painting colour.
type colour struct{ on, off string }

var (
	dim     = colour{"\x1b[2m", "\x1b[22m"}
	bold    = colour{"\x1b[1m", "\x1b[22m"}
	cyan    = colour{"\x1b[36m", "\x1b[39m"}
	green   = colour{"\x1b[32m", "\x1b[39m"}
	red     = colour{"\x1b[31m", "\x1b[39m"}
	yellow  = colour{"\x1b[33m", "\x1b[39m"}
	magenta = colour{"\x1b[35m", "\x1b[39m"}
)

// renderer paints a block of lines that is rewritten in place, with a log of
// permanent lines scrolling above it.
//
// The contract with the terminal is simple: every line written to the live
// region is truncated to the terminal width, so one line of output is always
// one row on screen. That keeps the cursor-up count exact — the moment a
// line wraps, the arithmetic is wrong and the display starts eating itself.
type renderer struct {
	out     *bufio.Writer
	width   int
	animate bool
	colour  bool

	rows    int // rows the live region currently occupies
	spinner int
	stopped bool
}

func newRenderer(out io.Writer, width int, animate, useColour bool) *renderer {
	if width < 40 {
		width = 40
	}
	r := &renderer{out: bufio.NewWriter(out), width: width, animate: animate, colour: useColour}
	if animate {
		fmt.Fprint(r.out, ansiHideCursor)
		r.out.Flush()
	}
	return r
}

// paint replaces the live region with lines.
func (r *renderer) paint(lines []string) {
	if !r.animate {
		return
	}
	r.clear()
	for i, line := range lines {
		if i > 0 {
			fmt.Fprint(r.out, "\n")
		}
		fmt.Fprint(r.out, ansiClearLine, r.fit(line))
	}
	r.rows = len(lines)
	r.out.Flush()
}

// note writes a line that stays on screen, above the live region.
func (r *renderer) note(line string) {
	if !r.animate {
		// No live region to displace, and no terminal to send control
		// codes to — a pipe or a log file gets the plain line.
		fmt.Fprintln(r.out, line)
		r.out.Flush()
		return
	}
	r.clear()
	fmt.Fprint(r.out, ansiClearLine, r.fit(line), "\n")
	r.rows = 0
	r.out.Flush()
}

// clear erases the live region and parks the cursor at its first column.
func (r *renderer) clear() {
	if !r.animate {
		return
	}
	if r.rows == 0 {
		fmt.Fprint(r.out, ansiLineStart)
		return
	}
	for i := 0; i < r.rows-1; i++ {
		fmt.Fprint(r.out, ansiClearLine, "\x1b[1A")
	}
	fmt.Fprint(r.out, ansiClearLine, ansiLineStart)
	r.rows = 0
}

// stop tears the live region down and gives the cursor back. It is
// idempotent: the run loop stops the display before printing its summary,
// and the deferred call then has nothing left to do.
func (r *renderer) stop() {
	if r.stopped {
		return
	}
	r.stopped = true
	r.clear()
	if r.animate {
		fmt.Fprint(r.out, ansiShowCursor)
	}
	r.out.Flush()
}

// tick advances the spinner and returns the current frame.
func (r *renderer) tick() string {
	r.spinner = (r.spinner + 1) % len(spinnerFrames)
	return spinnerFrames[r.spinner]
}

// fit trims a line to the terminal width, counting only printable runes so
// the escape sequences that colour it do not eat the budget.
func (r *renderer) fit(line string) string {
	if visibleWidth(line) <= r.width {
		return line
	}
	var b strings.Builder
	width, inEscape := 0, false
	for _, ch := range line {
		if inEscape {
			b.WriteRune(ch)
			if ch == 'm' {
				inEscape = false
			}
			continue
		}
		if ch == '\x1b' {
			inEscape = true
			b.WriteRune(ch)
			continue
		}
		if width >= r.width-1 {
			b.WriteString("…")
			break
		}
		b.WriteRune(ch)
		width++
	}
	// Whatever colour the truncated tail was carrying has to be closed, or
	// it bleeds into the rest of the screen.
	b.WriteString("\x1b[0m")
	return b.String()
}

// paintColour wraps s in an SGR pair, or returns it untouched when colour is
// off. Named to avoid colliding with the colour type.
func (r *renderer) paintColour(c colour, s string) string {
	if !r.colour || s == "" {
		return s
	}
	return c.on + s + c.off
}

// visibleWidth counts runes that occupy a cell, skipping ANSI sequences.
func visibleWidth(s string) int {
	width, inEscape := 0, false
	for _, ch := range s {
		switch {
		case inEscape:
			if ch == 'm' {
				inEscape = false
			}
		case ch == '\x1b':
			inEscape = true
		default:
			width++
		}
	}
	return width
}
