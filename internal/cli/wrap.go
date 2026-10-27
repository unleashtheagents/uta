package cli

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// termWidth returns the column width of w when it is a terminal, 0 when it
// is not (no wrapping). Output wrapped to this width never soft-wraps in
// the terminal, which keeps decorated lines (gutters, indents) aligned and
// keeps screen-scraping tools (recorders, CI logs) coherent.
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil || cols <= 0 {
		return 0
	}
	return cols
}

// wrapText word-wraps s to width columns (per line, preserving existing
// newlines). width <= 0 returns s unchanged. Words longer than the width
// are hard-broken rather than overflowed.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		col := 0
		for _, word := range strings.Split(line, " ") {
			wlen := len([]rune(word))
			switch {
			case col == 0:
				// First word on the line: hard-break if it alone overflows.
				for wlen > width {
					r := []rune(word)
					out.WriteString(string(r[:width]))
					out.WriteByte('\n')
					word = string(r[width:])
					wlen = len(r) - width
				}
				out.WriteString(word)
				col = wlen
			case col+1+wlen <= width:
				out.WriteByte(' ')
				out.WriteString(word)
				col += 1 + wlen
			default:
				out.WriteByte('\n')
				for wlen > width {
					r := []rune(word)
					out.WriteString(string(r[:width]))
					out.WriteByte('\n')
					word = string(r[width:])
					wlen = len(r) - width
				}
				out.WriteString(word)
				col = wlen
			}
		}
	}
	return out.String()
}

// printWrapped writes s to w, word-wrapped to the terminal width when w is
// a terminal, followed by a newline.
func printWrapped(w io.Writer, s string) {
	width := termWidth(w)
	if width > 8 {
		s = wrapText(s, width-1)
	}
	io.WriteString(w, s)
	io.WriteString(w, "\n")
}
