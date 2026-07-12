package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"
)

// errInterrupted is returned by ReadLine when the user presses Ctrl-C at
// the prompt: the shell clears the line and re-prompts instead of dying.
var errInterrupted = errors.New("interrupted")

// lineReader is the shell's input seam. The interactive implementation is
// a raw-mode line editor with history; the fallback wraps a bufio.Scanner
// so pipes, scripts, and tests keep byte-identical behavior.
type lineReader interface {
	// ReadLine displays prompt and returns one line without the trailing
	// newline. io.EOF ends the shell; errInterrupted re-prompts.
	ReadLine(prompt string) (string, error)
	// Remember records an executed line into history (interactive only).
	Remember(line string)
	// Close flushes history to disk (interactive only).
	Close()
}

// newLineReader picks the right implementation: the editor when stdin is a
// real terminal, the scanner otherwise. historyPath may be empty (no
// persistence).
func newLineReader(in io.Reader, out io.Writer, historyPath string) lineReader {
	if f, ok := in.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
		if fOut, ok := out.(*os.File); ok && isatty.IsTerminal(fOut.Fd()) {
			return newTermLineReader(f, fOut, historyPath)
		}
	}
	return &scannerLineReader{out: out, sc: newShellScanner(in)}
}

func newShellScanner(in io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return sc
}

type scannerLineReader struct {
	out io.Writer
	sc  *bufio.Scanner
}

func (s *scannerLineReader) ReadLine(prompt string) (string, error) {
	fmt.Fprint(s.out, prompt)
	if !s.sc.Scan() {
		fmt.Fprintln(s.out)
		if err := s.sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return s.sc.Text(), nil
}

func (s *scannerLineReader) Remember(string) {}
func (s *scannerLineReader) Close()          {}

// termLineReader is the interactive path: per ReadLine it puts the
// terminal into raw mode, runs the editor, and restores cooked mode before
// returning — so everything the turn prints afterwards behaves normally.
type termLineReader struct {
	in          *os.File
	out         *os.File
	history     []string
	historyPath string
}

// maxHistory bounds both the in-memory ring and the persisted file.
const maxHistory = 1000

func newTermLineReader(in, out *os.File, historyPath string) *termLineReader {
	r := &termLineReader{in: in, out: out, historyPath: historyPath}
	if historyPath != "" {
		if data, err := os.ReadFile(historyPath); err == nil {
			for _, ln := range strings.Split(string(data), "\n") {
				if ln = strings.TrimRight(ln, "\r"); ln != "" {
					r.history = append(r.history, ln)
				}
			}
			if len(r.history) > maxHistory {
				r.history = r.history[len(r.history)-maxHistory:]
			}
		}
	}
	return r
}

func (r *termLineReader) ReadLine(prompt string) (string, error) {
	old, err := term.MakeRaw(int(r.in.Fd()))
	if err != nil {
		// Terminal refused raw mode — degrade to plain reads.
		fmt.Fprint(r.out, prompt)
		sc := newShellScanner(r.in)
		if !sc.Scan() {
			return "", io.EOF
		}
		return sc.Text(), nil
	}
	defer term.Restore(int(r.in.Fd()), old)
	line, rerr := editLine(r.in, r.out, prompt, r.history)
	return line, rerr
}

func (r *termLineReader) Remember(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if n := len(r.history); n > 0 && r.history[n-1] == line {
		return // collapse consecutive duplicates
	}
	r.history = append(r.history, line)
	if len(r.history) > maxHistory {
		r.history = r.history[len(r.history)-maxHistory:]
	}
}

func (r *termLineReader) Close() {
	if r.historyPath == "" || len(r.history) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.historyPath), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(r.historyPath, []byte(strings.Join(r.history, "\n")+"\n"), 0o600)
}

// editLine is the pure editor: it reads key bytes from r, renders onto w
// (which must be in raw mode when w is a real terminal), and returns the
// finished line. It is deliberately io-only — tests drive it with byte
// slices and assert results without a TTY.
//
// Supported keys: printable runes (UTF-8), Enter, Backspace, Delete,
// ←/→/Home/End (and Ctrl-B/F/A/E), ↑/↓ history (Ctrl-P/N), Ctrl-K/U/W
// kills, Ctrl-L clear screen, Ctrl-C interrupt, Ctrl-D EOF on empty line.
func editLine(r io.Reader, w io.Writer, prompt string, history []string) (string, error) {
	br := bufio.NewReader(r)
	var buf []rune
	cur := 0
	histIdx := len(history) // one past the end = "the line being typed"
	var pending []rune      // the in-progress line, saved while browsing history

	redraw := func() {
		fmt.Fprintf(w, "\r%s%s\x1b[K", prompt, string(buf))
		if back := len(buf) - cur; back > 0 {
			fmt.Fprintf(w, "\x1b[%dD", back)
		}
	}
	setLine := func(s string) {
		buf = []rune(s)
		cur = len(buf)
		redraw()
	}

	redraw()
	for {
		c, err := br.ReadByte()
		if err != nil {
			fmt.Fprint(w, "\r\n")
			if len(buf) > 0 {
				return string(buf), nil
			}
			return "", io.EOF
		}
		switch c {
		case '\r', '\n':
			fmt.Fprint(w, "\r\n")
			return string(buf), nil
		case 0x03: // Ctrl-C
			fmt.Fprint(w, "^C\r\n")
			return "", errInterrupted
		case 0x04: // Ctrl-D
			if len(buf) == 0 {
				fmt.Fprint(w, "\r\n")
				return "", io.EOF
			}
			if cur < len(buf) { // delete-forward on non-empty line
				buf = append(buf[:cur], buf[cur+1:]...)
				redraw()
			}
		case 0x7f, 0x08: // Backspace
			if cur > 0 {
				buf = append(buf[:cur-1], buf[cur:]...)
				cur--
				redraw()
			}
		case 0x01: // Ctrl-A
			cur = 0
			redraw()
		case 0x05: // Ctrl-E
			cur = len(buf)
			redraw()
		case 0x02: // Ctrl-B
			if cur > 0 {
				cur--
				redraw()
			}
		case 0x06: // Ctrl-F
			if cur < len(buf) {
				cur++
				redraw()
			}
		case 0x0b: // Ctrl-K — kill to end
			buf = buf[:cur]
			redraw()
		case 0x15: // Ctrl-U — kill to start
			buf = append([]rune{}, buf[cur:]...)
			cur = 0
			redraw()
		case 0x17: // Ctrl-W — delete word before cursor
			i := cur
			for i > 0 && buf[i-1] == ' ' {
				i--
			}
			for i > 0 && buf[i-1] != ' ' {
				i--
			}
			buf = append(buf[:i], buf[cur:]...)
			cur = i
			redraw()
		case 0x0c: // Ctrl-L — clear screen, keep line
			fmt.Fprint(w, "\x1b[H\x1b[2J")
			redraw()
		case 0x10: // Ctrl-P — history up
			if histIdx > 0 {
				if histIdx == len(history) {
					pending = append([]rune{}, buf...)
				}
				histIdx--
				setLine(history[histIdx])
			}
		case 0x0e: // Ctrl-N — history down
			if histIdx < len(history) {
				histIdx++
				if histIdx == len(history) {
					setLine(string(pending))
				} else {
					setLine(history[histIdx])
				}
			}
		case 0x1b: // ESC sequences
			b1, err := br.ReadByte()
			if err != nil {
				continue
			}
			if b1 != '[' && b1 != 'O' {
				continue // bare ESC or unknown — ignore
			}
			b2, err := br.ReadByte()
			if err != nil {
				continue
			}
			switch b2 {
			case 'A': // up
				if histIdx > 0 {
					if histIdx == len(history) {
						pending = append([]rune{}, buf...)
					}
					histIdx--
					setLine(history[histIdx])
				}
			case 'B': // down
				if histIdx < len(history) {
					histIdx++
					if histIdx == len(history) {
						setLine(string(pending))
					} else {
						setLine(history[histIdx])
					}
				}
			case 'C': // right
				if cur < len(buf) {
					cur++
					redraw()
				}
			case 'D': // left
				if cur > 0 {
					cur--
					redraw()
				}
			case 'H': // Home
				cur = 0
				redraw()
			case 'F': // End
				cur = len(buf)
				redraw()
			case '1', '4', '3', '7', '8': // vt-style: 1~ Home, 4~/8~ End, 3~ Delete, 7~ Home
				tilde, err := br.ReadByte()
				if err != nil || tilde != '~' {
					continue
				}
				switch b2 {
				case '1', '7':
					cur = 0
				case '4', '8':
					cur = len(buf)
				case '3':
					if cur < len(buf) {
						buf = append(buf[:cur], buf[cur+1:]...)
					}
				}
				redraw()
			}
		default:
			if c < 0x20 {
				continue // unhandled control byte
			}
			r0, size := decodeUTF8Head(c)
			bs := make([]byte, 1, size)
			bs[0] = c
			for len(bs) < size {
				nb, err := br.ReadByte()
				if err != nil {
					break
				}
				bs = append(bs, nb)
			}
			ru := r0
			if size > 1 {
				ru, _ = utf8.DecodeRune(bs)
			}
			if ru == utf8.RuneError && size > 1 {
				continue
			}
			buf = append(buf[:cur], append([]rune{ru}, buf[cur:]...)...)
			cur++
			redraw()
		}
	}
}

// decodeUTF8Head returns the rune for a single-byte encoding, or the total
// sequence length when b starts a multi-byte one.
func decodeUTF8Head(b byte) (rune, int) {
	switch {
	case b < 0x80:
		return rune(b), 1
	case b&0xE0 == 0xC0:
		return 0, 2
	case b&0xF0 == 0xE0:
		return 0, 3
	case b&0xF8 == 0xF0:
		return 0, 4
	default:
		return utf8.RuneError, 1
	}
}
