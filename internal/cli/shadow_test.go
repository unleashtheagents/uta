package cli

import (
	"testing"
	"time"
)

func TestParseSinceDuration(t *testing.T) {
	day := 24 * time.Hour
	week := 7 * day

	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"whitespace-only", "   ", 0, false},
		{"days-shorthand", "7d", 7 * day, false},
		{"weeks-shorthand", "2w", 2 * week, false},
		{"zero-days", "0d", 0, false},
		{"one-day", "1d", day, false},
		{"large-days", "365d", 365 * day, false},
		{"go-hours", "24h", 24 * time.Hour, false},
		{"go-minutes", "30m", 30 * time.Minute, false},
		{"go-composite", "1h30m", time.Hour + 30*time.Minute, false},
		{"trimmed", "  7d  ", 7 * day, false},
		{"unknown-unit", "7y", 0, true},
		{"non-numeric-shorthand", "abcd", 0, true},
		{"negative-days", "-1d", 0, true},
		{"bare-junk", "junk", 0, true},
		{"unit-only", "d", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSinceDuration(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSinceDuration(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSinceDuration(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseSinceDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitDurationShorthand(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantN    int
		wantUnit string
		wantOK   bool
	}{
		{"days", "7d", 7, "d", true},
		{"weeks", "2w", 2, "w", true},
		{"zero-days", "0d", 0, "d", true},
		{"multi-digit", "365d", 365, "d", true},
		{"empty", "", 0, "", false},
		{"too-short", "d", 0, "", false},
		{"hours-not-shorthand", "24h", 0, "", false},
		{"minutes-not-shorthand", "30m", 0, "", false},
		{"unknown-unit", "7y", 0, "", false},
		{"negative", "-1d", 0, "", false},
		{"non-numeric", "abcd", 0, "", false},
		{"trailing-junk", "7dx", 0, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, unit, ok := splitDurationShorthand(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("splitDurationShorthand(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if n != tc.wantN || unit != tc.wantUnit {
				t.Fatalf("splitDurationShorthand(%q) = (%d, %q), want (%d, %q)",
					tc.in, n, unit, tc.wantN, tc.wantUnit)
			}
		})
	}
}
