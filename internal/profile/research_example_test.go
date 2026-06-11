package profile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
)

func TestResearchExampleProfileLoads(t *testing.T) {
	abs, _ := filepath.Abs("../../examples/profiles/research.yaml")
	p, err := Load(abs)
	if err != nil {
		t.Fatalf("load research example: %v", err)
	}
	if p.Name != "research" {
		t.Errorf("name = %q, want research", p.Name)
	}
	if len(p.Personas) == 0 {
		t.Error("personas should be non-empty")
	}
	// The audit promised both personas explicitly. Pin them so a future
	// edit that drops one is caught.
	want := map[string]bool{"deep-researcher": false, "citation-curator": false}
	for _, id := range p.Personas {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("research profile is missing persona %q", id)
		}
	}

	// Tavily MCP server must be wired with TAVILY_API_KEY in the env so
	// the bridge can launch it. If the entry disappears the audit's
	// "no Tavily adapter visible" gap re-opens.
	var tavily *ProfileMCPServer
	for i := range p.MCPServers {
		if p.MCPServers[i].Name == "tavily" {
			tavily = &p.MCPServers[i]
			break
		}
	}
	if tavily == nil {
		t.Fatal("research profile is missing the tavily MCP server")
	}
	if v, ok := tavily.Env["TAVILY_API_KEY"]; !ok || !strings.Contains(v, "TAVILY_API_KEY") {
		t.Errorf("tavily server env must thread TAVILY_API_KEY through (got %q)", v)
	}
}

// TestResearchPersonas_ChartersExercised loads the example deep-researcher
// and citation-curator persona YAMLs and asserts their charters carry the
// invariants the researchdb writer enforces. Without this test, the
// research-mode "personas" are just file-existence checks; the audit
// flagged exactly that gap.
func TestResearchPersonas_ChartersExercised(t *testing.T) {
	personasDir, _ := filepath.Abs("../../examples/personas")
	cases := []struct {
		file        string
		wantID      string
		wantTitle   string
		wantTag     string
		mustContain []string
	}{
		{
			file:      "deep-researcher.yaml",
			wantID:    "deep-researcher",
			wantTitle: "researcher",
			wantTag:   "research",
			// The deep-researcher must tell the worker WHERE to write
			// (RESEARCH_DATABASE.md) and that the retrieval timestamp is
			// captured at fetch time — both invariants Write enforces.
			mustContain: []string{
				"RESEARCH_DATABASE.md",
				"retrieval timestamp",
			},
		},
		{
			file:      "citation-curator.yaml",
			wantID:    "citation-curator",
			wantTitle: "curator",
			wantTag:   "research",
			// The citation-curator's whole point is the "no URL +
			// no timestamp = no claim" rule. If that string ever drifts
			// off the prompt, the charter has been silently broken.
			mustContain: []string{
				"NO URL",
				"NO CLAIM",
				"RESEARCH_DATABASE.md",
			},
		},
	}
	for _, c := range cases {
		p, err := config.LoadPersona(filepath.Join(personasDir, c.file))
		if err != nil {
			t.Errorf("load %s: %v", c.file, err)
			continue
		}
		if p.ID != c.wantID {
			t.Errorf("%s: id = %q, want %q", c.file, p.ID, c.wantID)
		}
		if !strings.Contains(strings.ToLower(p.Title), c.wantTitle) {
			t.Errorf("%s: title %q should contain %q", c.file, p.Title, c.wantTitle)
		}
		tagSeen := false
		for _, tag := range p.Tags {
			if tag == c.wantTag {
				tagSeen = true
				break
			}
		}
		if !tagSeen {
			t.Errorf("%s: tags %v missing %q", c.file, p.Tags, c.wantTag)
		}
		for _, needle := range c.mustContain {
			if !strings.Contains(p.Prompt, needle) {
				t.Errorf("%s: prompt missing required charter phrase %q", c.file, needle)
			}
		}
	}
}

// TestResearchPersona_FileMissingErrors is the negative case for the
// charter loader: a bogus path must surface an error rather than silently
// returning a zero-valued persona. Without this, a typo in the profile's
// personas list would yield a no-op worker.
func TestResearchPersona_FileMissingErrors(t *testing.T) {
	if _, err := config.LoadPersona(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("LoadPersona on missing file: want error, got nil")
	}
}
