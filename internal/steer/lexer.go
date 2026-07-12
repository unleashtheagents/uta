package steer

import (
	"fmt"
	"strings"
)

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokNumber // integer literal; "80k" lexes as 80000
	tokString // "..." single-line string, unescaped
	tokPrompt // """...""" block, dedented
	tokLParen
	tokRParen
	tokLBrace
	tokRBrace
	tokLBracket
	tokRBracket
	tokComma
	tokColon
	tokArrow  // ->
	tokAssign // =
	tokLE     // <=
	tokDollar // $
)

var tokNames = map[tokKind]string{
	tokEOF: "end of file", tokIdent: "identifier", tokNumber: "number",
	tokString: "string", tokPrompt: "prompt block",
	tokLParen: "'('", tokRParen: "')'", tokLBrace: "'{'", tokRBrace: "'}'",
	tokLBracket: "'['", tokRBracket: "']'", tokComma: "','", tokColon: "':'",
	tokArrow: "'->'", tokAssign: "'='", tokLE: "'<='", tokDollar: "'$'",
}

type token struct {
	kind tokKind
	pos  Pos
	text string // ident text / raw string value
	num  int64  // for tokNumber
}

// lexer produces the token stream for one file. It tracks source lines so
// diagnostics can show excerpts.
type lexer struct {
	file  string
	src   string
	lines []string
	off   int
	line  int
	col   int
}

func newLexer(file, src string) *lexer {
	// A shebang line makes .steer files directly executable
	// (#!/usr/bin/env -S uta mission run); the lexer skips it wholesale so
	// it never collides with the grammar.
	return &lexer{file: file, src: src, lines: strings.Split(src, "\n"), line: 1, col: 1}
}

func (l *lexer) srcLine(n int) string {
	if n >= 1 && n <= len(l.lines) {
		return strings.TrimRight(l.lines[n-1], "\r")
	}
	return ""
}

func (l *lexer) errAt(p Pos, format string, args ...any) Diag {
	return Diag{File: l.file, Pos: p, Msg: fmt.Sprintf(format, args...), SrcLine: l.srcLine(p.Line)}
}

func (l *lexer) peekByte() byte {
	if l.off >= len(l.src) {
		return 0
	}
	return l.src[l.off]
}

func (l *lexer) peekByteAt(delta int) byte {
	if l.off+delta >= len(l.src) {
		return 0
	}
	return l.src[l.off+delta]
}

func (l *lexer) advance() byte {
	c := l.src[l.off]
	l.off++
	if c == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return c
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// next returns the next token, skipping whitespace, // comments, and a
// leading #! shebang line.
func (l *lexer) next() (token, *Diag) {
	for l.off < len(l.src) {
		c := l.peekByte()
		switch {
		case c == '#' && l.off == 0 && l.peekByteAt(1) == '!':
			for l.off < len(l.src) && l.peekByte() != '\n' {
				l.advance()
			}
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			l.advance()
		case c == '/' && l.peekByteAt(1) == '/':
			for l.off < len(l.src) && l.peekByte() != '\n' {
				l.advance()
			}
		default:
			goto scan
		}
	}
	return token{kind: tokEOF, pos: Pos{l.line, l.col}}, nil

scan:
	pos := Pos{l.line, l.col}
	c := l.peekByte()

	switch {
	case isIdentStart(c):
		start := l.off
		for l.off < len(l.src) && isIdentPart(l.peekByte()) {
			l.advance()
		}
		return token{kind: tokIdent, pos: pos, text: l.src[start:l.off]}, nil

	case isDigit(c):
		var n int64
		for l.off < len(l.src) && isDigit(l.peekByte()) {
			n = n*10 + int64(l.advance()-'0')
		}
		// "80k" → 80000, but "2min" leaves "min" for the ident scanner.
		if l.peekByte() == 'k' && !isIdentPart(l.peekByteAt(1)) {
			l.advance()
			n *= 1000
		}
		return token{kind: tokNumber, pos: pos, num: n}, nil

	case c == '"':
		if strings.HasPrefix(l.src[l.off:], `"""`) {
			return l.lexPromptBlock(pos)
		}
		return l.lexString(pos)
	}

	l.advance()
	switch c {
	case '(':
		return token{kind: tokLParen, pos: pos}, nil
	case ')':
		return token{kind: tokRParen, pos: pos}, nil
	case '{':
		return token{kind: tokLBrace, pos: pos}, nil
	case '}':
		return token{kind: tokRBrace, pos: pos}, nil
	case '[':
		return token{kind: tokLBracket, pos: pos}, nil
	case ']':
		return token{kind: tokRBracket, pos: pos}, nil
	case ',':
		return token{kind: tokComma, pos: pos}, nil
	case ':':
		return token{kind: tokColon, pos: pos}, nil
	case '$':
		return token{kind: tokDollar, pos: pos}, nil
	case '=':
		return token{kind: tokAssign, pos: pos}, nil
	case '-':
		if l.peekByte() == '>' {
			l.advance()
			return token{kind: tokArrow, pos: pos}, nil
		}
		d := l.errAt(pos, "unexpected '-' (only '->' is valid here)")
		return token{}, &d
	case '<':
		if l.peekByte() == '=' {
			l.advance()
			return token{kind: tokLE, pos: pos}, nil
		}
		d := l.errAt(pos, "unexpected '<' (only '<=' is valid here)")
		return token{}, &d
	}
	d := l.errAt(pos, "unexpected character %q", string(c))
	return token{}, &d
}

func (l *lexer) lexString(pos Pos) (token, *Diag) {
	l.advance() // opening quote
	var b strings.Builder
	for l.off < len(l.src) {
		c := l.peekByte()
		if c == '\n' {
			d := l.errAt(pos, "unterminated string (single-quoted strings cannot span lines; use a prompt block)")
			return token{}, &d
		}
		l.advance()
		if c == '"' {
			return token{kind: tokString, pos: pos, text: b.String()}, nil
		}
		if c == '\\' && l.off < len(l.src) {
			e := l.advance()
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"', '\\':
				b.WriteByte(e)
			default:
				d := l.errAt(pos, `unknown escape \%s in string`, string(e))
				return token{}, &d
			}
			continue
		}
		b.WriteByte(c)
	}
	d := l.errAt(pos, "unterminated string")
	return token{}, &d
}

// lexPromptBlock reads """…""", strips the common leading indentation, and
// trims one leading/trailing blank line so authors can format naturally.
func (l *lexer) lexPromptBlock(pos Pos) (token, *Diag) {
	l.advance()
	l.advance()
	l.advance() // """
	start := l.off
	for l.off < len(l.src) {
		if strings.HasPrefix(l.src[l.off:], `"""`) {
			raw := l.src[start:l.off]
			l.advance()
			l.advance()
			l.advance()
			return token{kind: tokPrompt, pos: pos, text: dedent(raw)}, nil
		}
		l.advance()
	}
	d := l.errAt(pos, `unterminated prompt block (missing closing """)`)
	return token{}, &d
}

func dedent(s string) string {
	lines := strings.Split(s, "\n")
	// Drop a leading line that is empty (the newline right after """).
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	// Drop a trailing whitespace-only line (the indentation before """).
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	indent := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent > 0 {
		for i, ln := range lines {
			if len(ln) >= indent {
				lines[i] = ln[indent:]
			} else {
				lines[i] = strings.TrimLeft(ln, " \t")
			}
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}
