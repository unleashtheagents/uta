package capability

import (
	"encoding/json"
	"testing"
)

func TestNewGate_NilAndEmpty(t *testing.T) {
	g := NewGate(nil, nil)
	if !g.Empty() {
		t.Errorf("nil/nil gate should be Empty()")
	}
	dec, _ := g.Inspect("Bash", nil)
	if dec != Allow {
		t.Errorf("empty gate must Allow, got %v", dec)
	}

	var nilGate *Gate
	if !nilGate.Empty() {
		t.Errorf("nil gate must report Empty()")
	}
	dec, _ = nilGate.Inspect("Bash", nil)
	if dec != Allow {
		t.Errorf("nil gate Inspect must Allow, got %v", dec)
	}
}

func TestGate_DeniedToolName_Blocks(t *testing.T) {
	g := NewGate(nil, []string{"Bash(curl *)"})
	input := json.RawMessage(`{"command":"curl https://example.com"}`)
	dec, pat := g.Inspect("Bash", input)
	if dec != Deny {
		t.Fatalf("expected Deny for Bash(curl *), got %v", dec)
	}
	if pat != "Bash(curl *)" {
		t.Errorf("matched pattern: got %q want %q", pat, "Bash(curl *)")
	}
}

func TestGate_DeniedToolName_DoesNotMatchDifferentCommand(t *testing.T) {
	g := NewGate(nil, []string{"Bash(curl *)"})
	input := json.RawMessage(`{"command":"ls -la"}`)
	dec, _ := g.Inspect("Bash", input)
	if dec != Allow {
		t.Errorf("Bash(ls -la) should pass when only Bash(curl *) is denied, got %v", dec)
	}
}

func TestGate_DenyWinsOverAllow(t *testing.T) {
	g := NewGate([]string{"Bash"}, []string{"Bash(curl *)"})
	input := json.RawMessage(`{"command":"curl https://example.com"}`)
	dec, pat := g.Inspect("Bash", input)
	if dec != Deny {
		t.Errorf("deny should win over allow; got %v", dec)
	}
	if pat != "Bash(curl *)" {
		t.Errorf("matched pattern: got %q", pat)
	}
}

func TestGate_AllowListMiss_Denies(t *testing.T) {
	g := NewGate([]string{"Read", "Edit"}, nil)
	dec, pat := g.Inspect("Bash", json.RawMessage(`{"command":"ls"}`))
	if dec != Deny {
		t.Errorf("Bash not on allow-list should deny, got %v", dec)
	}
	if pat != "" {
		t.Errorf("allow-miss deny has no specific pattern; got %q", pat)
	}
}

func TestGate_ToolNameOnlyPattern_AllowsAnyArgs(t *testing.T) {
	g := NewGate([]string{"Bash"}, nil)
	dec, _ := g.Inspect("Bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if dec != Allow {
		t.Errorf("'Bash' on allow list should match any args; got %v", dec)
	}
}

func TestGate_CaseInsensitiveToolName(t *testing.T) {
	g := NewGate(nil, []string{"BASH(curl *)"})
	dec, _ := g.Inspect("Bash", json.RawMessage(`{"command":"curl https://x.com"}`))
	if dec != Deny {
		t.Errorf("tool name match should be case-insensitive; got %v", dec)
	}
}

func TestGate_ArgGlobAcrossSeparators(t *testing.T) {
	// filepath.Match's "*" stops at separators; ours must not, since URLs
	// and paths contain slashes that we want to match through.
	g := NewGate(nil, []string{"Bash(curl *)"})
	input := json.RawMessage(`{"command":"curl https://api.example.com/v1/resource"}`)
	dec, _ := g.Inspect("Bash", input)
	if dec != Deny {
		t.Errorf("glob '*' must match across '/' too; got %v", dec)
	}
}

func TestGate_QuestionMarkWildcard(t *testing.T) {
	g := NewGate(nil, []string{"Bash(cur?)"})
	dec, _ := g.Inspect("Bash", json.RawMessage(`{"command":"curl"}`))
	if dec != Deny {
		t.Errorf("'?' should match a single char; got %v", dec)
	}
	dec, _ = g.Inspect("Bash", json.RawMessage(`{"command":"curls"}`))
	if dec != Allow {
		t.Errorf("'?' should match exactly one char; got %v on 'curls'", dec)
	}
}

func TestGate_NestedStringExtraction(t *testing.T) {
	g := NewGate(nil, []string{"Tool(secret-*)"})
	input := json.RawMessage(`{"args":{"flags":["-v","secret-abc"]}}`)
	dec, _ := g.Inspect("Tool", input)
	if dec != Deny {
		t.Errorf("nested string should be matched; got %v", dec)
	}
}

func TestGate_DroppedEmptyAndBadPatterns(t *testing.T) {
	g := NewGate([]string{"", "  ", "OK"}, []string{"   "})
	// All bad entries dropped; only "OK" remains.
	if len(g.allow) != 1 || len(g.deny) != 0 {
		t.Errorf("expected 1 allow, 0 deny after dropping bad patterns; got %d/%d",
			len(g.allow), len(g.deny))
	}
}

func TestGate_MalformedParens_Dropped(t *testing.T) {
	cases := []string{
		"Bash(curl",  // unclosed
		"Bash curl)", // stray close
		"(curl)",     // empty tool name
		"Bash((x))",  // nested
		"Bash(x)(y)", // multiple groups
		")Bash(",     // close before open
	}
	for _, raw := range cases {
		g := NewGate(nil, []string{raw})
		if len(g.deny) != 0 {
			t.Errorf("malformed pattern %q should be dropped; got %d deny entries", raw, len(g.deny))
		}
		// And critically: must not cause an unrelated tool call to be denied
		// (which would happen if the raw string survived as a dead-weight
		// tool-name pattern that matches nothing — fine — but also must not
		// inadvertently deny calls that should pass).
		dec, _ := g.Inspect("Bash", json.RawMessage(`{"command":"ls"}`))
		if dec != Allow {
			t.Errorf("malformed pattern %q should not affect unrelated calls; got %v", raw, dec)
		}
	}
}

func TestGate_MalformedDoesNotShadowValid(t *testing.T) {
	// A malformed deny pattern next to a valid one must not poison the
	// gate: the valid pattern still fires, the malformed one is dropped.
	g := NewGate(nil, []string{"Bash(curl", "Bash(rm *)"})
	dec, pat := g.Inspect("Bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if dec != Deny {
		t.Errorf("valid deny pattern should still fire; got %v", dec)
	}
	if pat != "Bash(rm *)" {
		t.Errorf("matched pattern: got %q want Bash(rm *)", pat)
	}
}

func TestGate_SecondDenyPatternMatches(t *testing.T) {
	// Inspect must iterate the full deny list, not bail after the first miss.
	g := NewGate(nil, []string{"Bash(curl *)", "Bash(rm *)"})
	dec, pat := g.Inspect("Bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if dec != Deny {
		t.Errorf("second deny pattern should fire; got %v", dec)
	}
	if pat != "Bash(rm *)" {
		t.Errorf("matched pattern: got %q want Bash(rm *)", pat)
	}
}

func TestGate_NilInputWithArgGlob_DoesNotMatch(t *testing.T) {
	// An arg-glob deny pattern must not fire when there are no input strings
	// to match against — otherwise empty/missing payloads would be denied
	// by every arg-glob pattern indiscriminately.
	g := NewGate(nil, []string{"Bash(rm *)"})
	dec, _ := g.Inspect("Bash", nil)
	if dec != Allow {
		t.Errorf("arg-glob with nil input should not match; got %v", dec)
	}
	// Same for empty JSON object — no strings to walk.
	dec, _ = g.Inspect("Bash", json.RawMessage(`{}`))
	if dec != Allow {
		t.Errorf("arg-glob with empty input should not match; got %v", dec)
	}
}

func TestExtractToolNameAndInput_ClaudeShape(t *testing.T) {
	payload := json.RawMessage(`{"name":"Bash","input":{"command":"ls"}}`)
	name, input := ExtractToolNameAndInput(payload)
	if name != "Bash" {
		t.Errorf("name: got %q want Bash", name)
	}
	if string(input) != `{"command":"ls"}` {
		t.Errorf("input: got %s", string(input))
	}
}

func TestExtractToolNameAndInput_GeminiShape(t *testing.T) {
	payload := json.RawMessage(`{"type":"tool_call","tool":"shell","args":{"cmd":"ls"}}`)
	name, input := ExtractToolNameAndInput(payload)
	if name != "shell" {
		t.Errorf("name: got %q want shell", name)
	}
	if string(input) != `{"cmd":"ls"}` {
		t.Errorf("input: got %s", string(input))
	}
}

func TestExtractToolNameAndInput_UnknownShape(t *testing.T) {
	payload := json.RawMessage(`{"unrelated":"value"}`)
	name, input := ExtractToolNameAndInput(payload)
	if name != "" {
		t.Errorf("name should be empty for unknown shape; got %q", name)
	}
	if string(input) != string(payload) {
		t.Errorf("fallback input should be the raw payload")
	}
}

func TestExtractToolNameAndInput_NotJSON(t *testing.T) {
	payload := json.RawMessage(`not-json`)
	name, input := ExtractToolNameAndInput(payload)
	if name != "" {
		t.Errorf("name from non-JSON should be empty; got %q", name)
	}
	if string(input) != string(payload) {
		t.Errorf("input from non-JSON should fall back to raw payload")
	}
}
