package cli

import (
	"strings"
	"testing"
)

func TestWrapText(t *testing.T) {
	if got := wrapText("short line", 80); got != "short line" {
		t.Fatalf("no-op wrap changed text: %q", got)
	}
	long := strings.Repeat("word ", 30) + "end"
	for _, ln := range strings.Split(wrapText(long, 40), "\n") {
		if n := len([]rune(ln)); n > 40 {
			t.Fatalf("line over width: %d %q", n, ln)
		}
	}
	// Words survive wrapping in order.
	if strings.ReplaceAll(wrapText(long, 40), "\n", " ") != long {
		t.Fatal("wrap lost or reordered words")
	}
	// A monster token is hard-broken, not overflowed.
	tok := strings.Repeat("x", 95)
	for _, ln := range strings.Split(wrapText("a "+tok+" b", 40), "\n") {
		if len(ln) > 40 {
			t.Fatalf("hard break missing: %q", ln)
		}
	}
	// Existing newlines preserved.
	if got := wrapText("a\nb", 40); got != "a\nb" {
		t.Fatalf("newlines mangled: %q", got)
	}
	if got := wrapText("anything", 0); got != "anything" {
		t.Fatalf("width 0 must be a no-op, got %q", got)
	}
}
