// Package capability is the tool-level enforcement layer that turns a
// MissionProfile's AllowedTools / DeniedTools lists from "the pre-approve
// hint we forward to claude --allowedTools" into an actual gate applied to
// every tool invocation observed on a provider's event stream.
//
// v1 pattern grammar (matches what claude's --allowedTools accepts):
//
//	ToolName            -- matches any invocation of the named tool
//	ToolName(arg-glob)  -- matches when one of the input's string fields
//	                       matches arg-glob (* and ? as glob wildcards)
//
// DeniedTools wins over AllowedTools on collision: an explicit deny pattern
// always blocks, even when an allow pattern would also match.
//
// Out of scope for v1: per-call argument fingerprinting (e.g. distinguishing
// `git status` from `git push --force` semantically rather than by string
// pattern). Pattern matching against the raw input strings is the v1 story.
package capability

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Decision is the outcome of inspecting a tool call.
type Decision int

const (
	Allow Decision = iota
	Deny
)

// String renders the decision for logs and trajectory payloads.
func (d Decision) String() string {
	if d == Deny {
		return "deny"
	}
	return "allow"
}

// Gate decides whether a tool invocation may proceed. Build it once per
// run from the active profile's AllowedTools / DeniedTools and hand it to
// the supervisor's event drainer; the drainer calls Inspect on every
// observed tool_call.
type Gate struct {
	allow []*pattern
	deny  []*pattern
}

type pattern struct {
	raw      string
	toolName string
	argRE    *regexp.Regexp // nil when the pattern has no (arg-glob) section
}

// NewGate parses the supplied allow/deny lists. Patterns are validated
// permissively — entries that don't fit the grammar are dropped silently
// rather than failing the run, mirroring how the rest of the profile
// loader tolerates partial errors. Returns a non-nil *Gate even on empty
// inputs so callers can treat it uniformly.
func NewGate(allowed, denied []string) *Gate {
	g := &Gate{}
	for _, s := range allowed {
		if p := parsePattern(s); p != nil {
			g.allow = append(g.allow, p)
		}
	}
	for _, s := range denied {
		if p := parsePattern(s); p != nil {
			g.deny = append(g.deny, p)
		}
	}
	return g
}

// Empty reports whether the gate has no patterns at all. The supervisor
// uses this to skip the JSON-extract step on the hot path when no profile
// restrictions are configured.
func (g *Gate) Empty() bool {
	return g == nil || (len(g.allow) == 0 && len(g.deny) == 0)
}

// Inspect evaluates one tool call. Returns the decision and, on Deny, the
// raw pattern string that matched (empty when the call was denied by
// virtue of an allow-list miss rather than an explicit deny pattern).
func (g *Gate) Inspect(toolName string, input json.RawMessage) (Decision, string) {
	if g == nil {
		return Allow, ""
	}
	toolName = strings.TrimSpace(toolName)
	for _, p := range g.deny {
		if p.matches(toolName, input) {
			return Deny, p.raw
		}
	}
	if len(g.allow) == 0 {
		return Allow, ""
	}
	for _, p := range g.allow {
		if p.matches(toolName, input) {
			return Allow, ""
		}
	}
	// Allow-list is non-empty and nothing matched — deny by default.
	return Deny, ""
}

func parsePattern(s string) *pattern {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Malformed-paren guard: drop entries that don't match the grammar
	// (unclosed paren, stray ')', nested parens, empty tool name before
	// '('). Without this guard such entries become tool-name-only patterns
	// with the raw string as a tool name and silently never match anything.
	hasOpen := strings.Contains(s, "(")
	hasClose := strings.Contains(s, ")")
	if hasOpen != hasClose {
		return nil
	}
	if strings.Count(s, "(") > 1 || strings.Count(s, ")") > 1 {
		return nil
	}
	p := &pattern{raw: s, toolName: s}
	if hasOpen {
		i := strings.Index(s, "(")
		j := strings.Index(s, ")")
		if i == 0 || j != len(s)-1 || j < i {
			return nil
		}
		p.toolName = strings.TrimSpace(s[:i])
		argGlob := strings.TrimSpace(s[i+1 : j])
		if argGlob != "" {
			p.argRE = globToRegexp(argGlob)
		}
	}
	if p.toolName == "" {
		return nil
	}
	return p
}

func (p *pattern) matches(toolName string, input json.RawMessage) bool {
	if !strings.EqualFold(p.toolName, toolName) {
		return false
	}
	if p.argRE == nil {
		return true
	}
	for _, v := range extractStrings(input) {
		if p.argRE.MatchString(v) {
			return true
		}
	}
	return false
}

// globToRegexp converts a glob (with * and ? wildcards) into a regexp
// anchored to the whole input string. Other regex metacharacters in the
// glob are quoted so they don't smuggle in unintended semantics.
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// extractStrings walks the JSON tool input and returns every string value
// it encounters (depth-first). v1 is intentionally schema-agnostic: every
// provider names the tool-input field differently (claude uses "command",
// gemini uses "args", declarative providers use whatever the YAML
// descriptor says), so matching against every string keeps the gate
// robust across providers at the cost of a small false-positive risk on
// pathological inputs.
func extractStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(v)
	return out
}

// ExtractToolNameAndInput pulls a normalized (name, input) pair out of a
// provider tool_call payload. Different providers use different field
// names (claude: "name"+"input"; gemini: "tool_call" with various
// shapes; declarative: whatever the YAML descriptor said), so we look
// across the common spellings and fall back to the whole payload as the
// input when no nested input field is present.
func ExtractToolNameAndInput(payload json.RawMessage) (string, json.RawMessage) {
	if len(payload) == 0 {
		return "", nil
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(payload, &generic); err != nil {
		return "", payload
	}
	var name string
	for _, k := range []string{"name", "tool", "tool_name", "toolName"} {
		v, ok := generic[k]
		if !ok {
			continue
		}
		var candidate string
		if err := json.Unmarshal(v, &candidate); err != nil {
			continue
		}
		if candidate != "" {
			name = candidate
			break
		}
	}
	for _, k := range []string{"input", "args", "arguments", "parameters"} {
		if v, ok := generic[k]; ok && len(v) > 0 {
			return name, v
		}
	}
	return name, payload
}
