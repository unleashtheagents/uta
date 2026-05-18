package engine

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Fatalf("under-max: got %q want short", got)
	}
	got := Truncate(strings.Repeat("a", 50), 10)
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Fatalf("expected first 10 a's, got %q", got)
	}
}

func TestTruncateUTF8Safe(t *testing.T) {
	// "héllo" — 'é' is 2 bytes (0xC3 0xA9). Cutting at byte index 2 would
	// land inside the multi-byte sequence and produce invalid UTF-8.
	got := Truncate("héllo world", 2)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "h") || strings.Contains(got, "\xC3") {
		t.Fatalf("expected clean rune-boundary cut, got %q", got)
	}
}

func TestTruncateWithMarker(t *testing.T) {
	if got := TruncateWithMarker("short", 100, "…"); got != "short" {
		t.Fatalf("under-max: got %q want short", got)
	}
	got := TruncateWithMarker(strings.Repeat("a", 50), 10, "…")
	if got != strings.Repeat("a", 10)+"…" {
		t.Fatalf("got %q", got)
	}
	// Empty marker == clean cut with no suffix.
	if got := TruncateWithMarker("hello world", 5, ""); got != "hello" {
		t.Fatalf("got %q want hello", got)
	}
}

func TestTruncateBytes(t *testing.T) {
	if got := TruncateBytes([]byte("hello"), 10); got != "hello" {
		t.Errorf("short input: got %q want hello", got)
	}
	if got := TruncateBytes([]byte("hello world"), 5); got != "hello" {
		t.Errorf("long input: got %q want hello", got)
	}
	if got := TruncateBytes(nil, 10); got != "" {
		t.Errorf("nil input: got %q want empty", got)
	}
	// UTF-8 boundary: 'é' is 2 bytes, max=2 should walk back to 'h'.
	got := TruncateBytes([]byte("héllo"), 2)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8: %q", got)
	}
	if got != "h" {
		t.Fatalf("got %q want h", got)
	}
}
