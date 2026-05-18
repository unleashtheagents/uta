package engine

import "strings"

// findJSONObjectByKey returns the substring of text that begins with a `{`
// whose first key is `"<key>"` and ends at the matching `}`. Brace matching
// is string-literal-aware: braces inside JSON string literals don't affect
// depth. Returns false when no such object is found or its braces don't
// balance.
//
// This lets parsers anchor on a known top-level key (e.g. "findings",
// "subtasks") so braces appearing in the LLM's prose preamble or trailer
// don't poison the extracted span.
func findJSONObjectByKey(text, key string) (string, bool) {
	needle := `"` + key + `"`
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		j := i + 1
		for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
			j++
		}
		if !strings.HasPrefix(text[j:], needle) {
			continue
		}
		if end := matchCloseBrace(text, i); end > i {
			return text[i : end+1], true
		}
	}
	return "", false
}

// matchCloseBrace returns the index of the `}` that closes the `{` at start.
// Returns -1 if no matching close is found. Strings (delimited by `"`) and
// their backslash escapes are honored so braces inside string literals don't
// count toward depth.
func matchCloseBrace(text string, start int) int {
	if start >= len(text) || text[start] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// extractJSONObject returns the first balanced `{...}` span in s using
// string-literal-aware brace matching, so braces inside JSON string literals
// don't affect depth and don't get mis-truncated by a stray `}` in trailing
// prose. Returns "" when no balanced shape is present.
//
// Tool adapters use this to tolerate warnings, log lines, or banners printed
// around the JSON payload they care about.
func extractJSONObject(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if end := matchCloseBrace(s, i); end > i {
			return s[i : end+1]
		}
	}
	return ""
}
