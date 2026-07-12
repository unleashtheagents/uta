package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// edit runs the editor over a scripted byte sequence and returns the line.
func edit(t *testing.T, keys string, history ...string) (string, error) {
	t.Helper()
	var render strings.Builder
	return editLine(strings.NewReader(keys), &render, "uta> ", history)
}

func TestEditLine_PlainTyping(t *testing.T) {
	line, err := edit(t, "hello world\r")
	if err != nil || line != "hello world" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestEditLine_BackspaceEdits(t *testing.T) {
	line, err := edit(t, "helpp\x7fo\r") // helpp<BS>o → helpo? no: helpp -> help -> helpo
	if err != nil || line != "helpo" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestEditLine_ArrowInsertInMiddle(t *testing.T) {
	// type "helo", ←, insert "l" → "hello"
	line, err := edit(t, "helo\x1b[Dl\r")
	if err != nil || line != "hello" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestEditLine_HomeEndDelete(t *testing.T) {
	// "xabc", Home, Delete → "abc"
	line, err := edit(t, "xabc\x1b[H\x1b[3~\r")
	if err != nil || line != "abc" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	// Ctrl-A / Ctrl-E round trip
	line, err = edit(t, "bc\x01a\x05d\r")
	if err != nil || line != "abcd" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestEditLine_Kills(t *testing.T) {
	// Ctrl-U kills to start
	line, err := edit(t, "discard this\x15keep\r")
	if err != nil || line != "keep" {
		t.Fatalf("Ctrl-U: line=%q err=%v", line, err)
	}
	// Ctrl-K kills to end: "keepdrop", Ctrl-A, →×4, Ctrl-K
	line, err = edit(t, "keepdrop\x01\x1b[C\x1b[C\x1b[C\x1b[C\x0b\r")
	if err != nil || line != "keep" {
		t.Fatalf("Ctrl-K: line=%q err=%v", line, err)
	}
	// Ctrl-W deletes the word before the cursor
	line, err = edit(t, "keep drop\x17\r")
	if err != nil || line != "keep " {
		t.Fatalf("Ctrl-W: line=%q err=%v", line, err)
	}
}

func TestEditLine_HistoryUpDown(t *testing.T) {
	hist := []string{"first goal", "second goal"}
	// ↑ → most recent entry
	line, err := edit(t, "\x1b[A\r", hist...)
	if err != nil || line != "second goal" {
		t.Fatalf("up1: line=%q err=%v", line, err)
	}
	// ↑↑ → older entry
	line, err = edit(t, "\x1b[A\x1b[A\r", hist...)
	if err != nil || line != "first goal" {
		t.Fatalf("up2: line=%q err=%v", line, err)
	}
	// typed text survives an up/down round trip
	line, err = edit(t, "draft\x1b[A\x1b[B\r", hist...)
	if err != nil || line != "draft" {
		t.Fatalf("roundtrip: line=%q err=%v", line, err)
	}
	// Ctrl-P behaves like ↑
	line, err = edit(t, "\x10\r", hist...)
	if err != nil || line != "second goal" {
		t.Fatalf("ctrl-p: line=%q err=%v", line, err)
	}
}

func TestEditLine_CtrlCInterrupts(t *testing.T) {
	_, err := edit(t, "half a line\x03")
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("err=%v, want errInterrupted", err)
	}
}

func TestEditLine_CtrlDSemantics(t *testing.T) {
	// empty line → EOF
	_, err := edit(t, "\x04")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want io.EOF", err)
	}
	// non-empty line → delete forward, not EOF: "ab", Home, Ctrl-D → "b"
	line, err := edit(t, "ab\x01\x04\r")
	if err != nil || line != "b" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestEditLine_UTF8Runes(t *testing.T) {
	line, err := edit(t, "héllo → 世界\r")
	if err != nil || line != "héllo → 世界" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	// backspace removes one rune, not one byte
	line, err = edit(t, "世界\x7f\r")
	if err != nil || line != "世" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestEditLine_EOFMidLineReturnsLine(t *testing.T) {
	line, err := edit(t, "partial")
	if err != nil || line != "partial" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestTermLineReader_HistoryPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "shell_history")
	r := &termLineReader{historyPath: path}
	r.Remember("first")
	r.Remember("second")
	r.Remember("second") // consecutive duplicate collapses
	r.Remember("  ")     // blank ignored
	r.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("history file: %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Errorf("history = %q", string(data))
	}

	r2 := newTermLineReader(nil, nil, path)
	if len(r2.history) != 2 || r2.history[1] != "second" {
		t.Errorf("reloaded history = %v", r2.history)
	}
}

func TestNewLineReader_NonTTYFallsBackToScanner(t *testing.T) {
	var out strings.Builder
	rl := newLineReader(strings.NewReader("hello\n"), &out, "")
	if _, ok := rl.(*scannerLineReader); !ok {
		t.Fatalf("reader = %T, want scanner fallback", rl)
	}
	line, err := rl.ReadLine("uta> ")
	if err != nil || line != "hello" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if !strings.Contains(out.String(), "uta> ") {
		t.Errorf("prompt not written: %q", out.String())
	}
}
