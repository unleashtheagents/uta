package researchdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if got == nil {
		t.Fatal("Load missing returned nil state")
	}
	if !got.Frontmatter.LastUpdated.IsZero() {
		t.Errorf("missing-file state should have zero LastUpdated, got %v", got.Frontmatter.LastUpdated)
	}
	if got.Body != "" {
		t.Errorf("missing-file state body = %q, want empty", got.Body)
	}
}

func TestWriteThenLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	retrieved := time.Date(2026, 5, 18, 9, 30, 0, 0, time.UTC)
	want := &State{
		Frontmatter: Frontmatter{
			Queries: []string{"mcp server authentication best practices"},
			Sources: []Source{
				{URL: "https://spec.modelcontextprotocol.io/authorization/", Title: "MCP authorization spec", Retrieved: retrieved},
				{URL: "https://example.org/mcp-oauth.html", Title: "OAuth in MCP", Retrieved: retrieved},
				{URL: "https://example.com/mcp-survey.pdf", Title: "Survey", Retrieved: retrieved},
			},
			Claims: []Claim{
				{Text: "MCP recommends OAuth 2.1 for remote servers.", Sources: []int{1, 2}},
				{Text: "stdio transports typically rely on environment-injected credentials.", Sources: []int{3}},
			},
		},
		Body: "## Synthesis\nOAuth 2.1 plus PKCE is the canonical recommendation.\n",
	}
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want.Frontmatter.LastUpdated.IsZero() {
		t.Fatal("Write did not stamp LastUpdated")
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Frontmatter.LastUpdated.IsZero() {
		t.Fatal("Load returned zero LastUpdated after roundtrip")
	}
	if len(got.Frontmatter.Sources) != 3 {
		t.Errorf("sources = %d, want 3", len(got.Frontmatter.Sources))
	}
	if len(got.Frontmatter.Claims) != 2 {
		t.Errorf("claims = %d, want 2", len(got.Frontmatter.Claims))
	}
	if got.Frontmatter.Sources[0].URL != "https://spec.modelcontextprotocol.io/authorization/" {
		t.Errorf("source[0].URL = %q", got.Frontmatter.Sources[0].URL)
	}
	if !got.Frontmatter.Sources[0].Retrieved.Equal(retrieved) {
		t.Errorf("source[0].Retrieved = %v, want %v", got.Frontmatter.Sources[0].Retrieved, retrieved)
	}
	if strings.TrimSpace(got.Body) != strings.TrimSpace(want.Body) {
		t.Errorf("body = %q, want %q", got.Body, want.Body)
	}
}

func TestWrite_RejectsSourceMissingURL(t *testing.T) {
	dir := t.TempDir()
	st := &State{Frontmatter: Frontmatter{
		Sources: []Source{{Title: "no url", Retrieved: time.Now().UTC()}},
	}}
	if err := Write(dir, st); err == nil {
		t.Error("expected url-missing source to error")
	}
}

func TestWrite_RejectsSourceMissingRetrieved(t *testing.T) {
	dir := t.TempDir()
	st := &State{Frontmatter: Frontmatter{
		Sources: []Source{{URL: "https://example.com"}},
	}}
	if err := Write(dir, st); err == nil {
		t.Error("expected retrieved-missing source to error")
	}
}

func TestWrite_RejectsUncitedClaim(t *testing.T) {
	dir := t.TempDir()
	st := &State{Frontmatter: Frontmatter{
		Sources: []Source{{URL: "https://example.com", Retrieved: time.Now().UTC()}},
		Claims:  []Claim{{Text: "no citations attached"}},
	}}
	if err := Write(dir, st); err == nil {
		t.Error("expected uncited claim to error")
	}
}

func TestWrite_RejectsClaimWithBadSourceIndex(t *testing.T) {
	dir := t.TempDir()
	st := &State{Frontmatter: Frontmatter{
		Sources: []Source{{URL: "https://example.com", Retrieved: time.Now().UTC()}},
		Claims:  []Claim{{Text: "cites a phantom", Sources: []int{99}}},
	}}
	if err := Write(dir, st); err == nil {
		t.Error("expected out-of-range citation to error")
	}
}

func TestAppendEntry_MergesAndRemaps(t *testing.T) {
	now := time.Now().UTC()
	st := &State{}
	st = AppendEntry(st,
		[]string{"first query"},
		[]Source{
			{URL: "https://a.example", Retrieved: now},
			{URL: "https://b.example", Retrieved: now},
		},
		[]Claim{{Text: "A and B agree", Sources: []int{1, 2}}},
		"",
	)
	if got := len(st.Frontmatter.Sources); got != 2 {
		t.Fatalf("after first append, sources = %d, want 2", got)
	}
	// Second entry: one duplicate source (a.example) and one new one (c.example).
	// Incoming claim cites incoming-1 (a.example) and incoming-2 (c.example);
	// after remap, claim must cite global-1 (existing a) and global-3 (new c).
	st = AppendEntry(st,
		[]string{"second query"},
		[]Source{
			{URL: "https://a.example", Retrieved: now},
			{URL: "https://c.example", Retrieved: now},
		},
		[]Claim{{Text: "A and C corroborate", Sources: []int{1, 2}}},
		"## Synthesis\nMore detail.\n",
	)
	if got := len(st.Frontmatter.Sources); got != 3 {
		t.Errorf("after second append, sources = %d, want 3 (duplicate URL must be deduped)", got)
	}
	if got := len(st.Frontmatter.Claims); got != 2 {
		t.Errorf("after second append, claims = %d, want 2", got)
	}
	last := st.Frontmatter.Claims[1]
	if len(last.Sources) != 2 || last.Sources[0] != 1 || last.Sources[1] != 3 {
		t.Errorf("remapped citations = %v, want [1 3]", last.Sources)
	}
	if !strings.Contains(st.Body, "More detail.") {
		t.Errorf("body did not pick up the appended synthesis: %q", st.Body)
	}
}

// TestRoundtrip_LoadModifyWriteLoad exercises the full Write → Load →
// AppendEntry → Write → Load cycle the deep-researcher persona executes
// across consecutive search-and-record turns. The first write establishes
// the ledger; the second write must preserve prior entries, deduplicate a
// source by URL, and remap incoming claim citations to the merged source
// list — all while staying parseable on the second Load.
func TestRoundtrip_LoadModifyWriteLoad(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC)
	first := &State{
		Frontmatter: Frontmatter{
			Queries: []string{"mcp authorization"},
			Sources: []Source{
				{URL: "https://spec.modelcontextprotocol.io/auth/", Title: "MCP auth", Retrieved: t0},
			},
			Claims: []Claim{
				{Text: "MCP recommends OAuth 2.1 for remote servers.", Sources: []int{1}},
			},
		},
		Body: "## Findings\nInitial pass [1].\n",
	}
	if err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if len(loaded.Frontmatter.Sources) != 1 {
		t.Fatalf("after first Load: sources=%d, want 1", len(loaded.Frontmatter.Sources))
	}
	firstStamp := loaded.Frontmatter.LastUpdated
	if firstStamp.IsZero() {
		t.Fatal("first Load: LastUpdated is zero")
	}

	// Modify: merge a second turn — one duplicate URL (must dedup) and one
	// new URL. The new claim's incoming citations are 1 (duplicate) and 2
	// (new), and must end up as 1 and 2 in the global ledger.
	t1 := t0.Add(time.Hour)
	loaded.Frontmatter.LastUpdated = time.Time{} // force Write to restamp
	merged := AppendEntry(loaded,
		[]string{"oauth pkce mcp"},
		[]Source{
			{URL: "https://spec.modelcontextprotocol.io/auth/", Title: "MCP auth", Retrieved: t1},
			{URL: "https://example.org/pkce.html", Title: "PKCE primer", Retrieved: t1},
		},
		[]Claim{{Text: "PKCE is required for public clients.", Sources: []int{1, 2}}},
		"## Findings\nSecond pass [1,2].\n",
	)
	if err := Write(dir, merged); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	again, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if got := len(again.Frontmatter.Sources); got != 2 {
		t.Errorf("after second Load: sources=%d, want 2 (duplicate URL must be deduped)", got)
	}
	if got := len(again.Frontmatter.Claims); got != 2 {
		t.Errorf("after second Load: claims=%d, want 2", got)
	}
	if got := len(again.Frontmatter.Queries); got != 2 {
		t.Errorf("after second Load: queries=%d, want 2", got)
	}
	last := again.Frontmatter.Claims[1]
	if len(last.Sources) != 2 || last.Sources[0] != 1 || last.Sources[1] != 2 {
		t.Errorf("after second Load: merged claim citations = %v, want [1 2]", last.Sources)
	}
	if again.Frontmatter.LastUpdated.Equal(firstStamp) {
		t.Errorf("second Write should restamp LastUpdated; both stamps = %v", firstStamp)
	}
	if !strings.Contains(again.Body, "Initial pass") || !strings.Contains(again.Body, "Second pass") {
		t.Errorf("after second Load: body lost prior turn or did not append new one: %q", again.Body)
	}
}

func TestParse_NoFrontmatterTreatedAsBody(t *testing.T) {
	dir := t.TempDir()
	raw := "just some notes, no frontmatter\n"
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Frontmatter.LastUpdated.IsZero() {
		t.Errorf("frontmatterless doc should have zero LastUpdated, got %v", got.Frontmatter.LastUpdated)
	}
	if got.Body != raw {
		t.Errorf("Body = %q, want %q", got.Body, raw)
	}
}

func TestParse_MalformedFrontmatterErrors(t *testing.T) {
	dir := t.TempDir()
	raw := "---\nlast_updated: not-a-timestamp\nsources: [a, b\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected malformed frontmatter to error, got nil")
	}
}

func TestPath_Format(t *testing.T) {
	got := Path("/tmp/ctx")
	want := "/tmp/ctx/" + Filename
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestLoad_EmptyDirErrors(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Error("expected empty contextDir to error")
	}
}

func TestWrite_NilStateErrors(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, nil); err == nil {
		t.Error("expected nil-state Write to error")
	}
}
