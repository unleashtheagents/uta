package engine

import "unicode/utf8"

// Truncate returns s unchanged when its byte length is <= max. Otherwise
// it returns the largest UTF-8-safe prefix of s that fits in max bytes,
// followed by "\n... [truncated]". The UTF-8 awareness prevents splitting
// a multi-byte rune mid-sequence, which would emit invalid UTF-8 that
// JSON encoders and terminals refuse to render.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return safeStringPrefix(s, max) + "\n... [truncated]"
}

// TruncateWithMarker is like Truncate but lets the caller pick the suffix
// marker (for example "…" for short labels, or "" to drop the marker
// entirely).
func TruncateWithMarker(s string, max int, marker string) string {
	if len(s) <= max {
		return s
	}
	return safeStringPrefix(s, max) + marker
}

// TruncateBytes returns the largest UTF-8-safe prefix of b that fits in
// max bytes, with no suffix marker.
func TruncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	end := max
	for end > 0 && !utf8.RuneStart(b[end]) {
		end--
	}
	return string(b[:end])
}

func safeStringPrefix(s string, max int) string {
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}
