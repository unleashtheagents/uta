package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/store"
)

func newRAGStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "rag.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st.DB)
}

// fakeEmbedder maps known phrases to fixed 4-dim vectors so KNN
// behavior is deterministic without any network.
type fakeEmbedder struct {
	name string
	vecs map[string][]float32
	def  []float32
}

func (f *fakeEmbedder) Name() string { return f.name }
func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		if v, ok := f.vecs[in]; ok {
			out[i] = v
		} else {
			out[i] = f.def
		}
	}
	return out, nil
}

func TestIndexAndSearch_FTSOnly(t *testing.T) {
	m := newRAGStore(t) // no embedder
	ctx := context.Background()

	docs := []string{
		"the gemini provider needs GEMINI_CLI_TRUST_WORKSPACE for headless runs",
		"reentrancy bugs come from external calls before state updates",
		"budget caps are enforced in supervisor run resume and reflector",
	}
	for _, d := range docs {
		if _, err := m.IndexDocument(ctx, Document{Kind: "fact", Body: d}); err != nil {
			t.Fatalf("IndexDocument: %v", err)
		}
	}

	res, err := m.SearchHybrid(ctx, "how do I fix a reentrancy vulnerability?", 2)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("no results")
	}
	if !strings.Contains(res[0].Body, "reentrancy") {
		t.Errorf("top result = %q, want the reentrancy doc", res[0].Body)
	}
	if len(res[0].Matched) != 1 || res[0].Matched[0] != "fts" {
		t.Errorf("matched = %v, want [fts] (no embedder configured)", res[0].Matched)
	}
}

func TestSearchHybrid_VectorContributes(t *testing.T) {
	// Two docs that DON'T share keywords with the query; the fake
	// embedder makes one of them the nearest neighbor. With FTS finding
	// nothing, the vector list must carry the result.
	emb := &fakeEmbedder{
		name: "fake/4d",
		vecs: map[string][]float32{
			"alpha doc about cooking pasta":  {1, 0, 0, 0},
			"beta doc about tuning engines":  {0, 1, 0, 0},
			"semantic query about carbonara": {0.95, 0.05, 0, 0}, // near alpha
		},
		def: []float32{0, 0, 1, 0},
	}
	m := newRAGStore(t)
	m.Embedder = emb
	ctx := context.Background()

	for _, d := range []string{"alpha doc about cooking pasta", "beta doc about tuning engines"} {
		if _, err := m.IndexDocument(ctx, Document{Kind: "fact", Body: d}); err != nil {
			t.Fatalf("IndexDocument: %v", err)
		}
	}

	res, err := m.SearchHybrid(ctx, "semantic query about carbonara", 1)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if !strings.Contains(res[0].Body, "pasta") {
		t.Errorf("top result = %q, want the pasta doc (nearest vector)", res[0].Body)
	}
	hasVec := false
	for _, mlabel := range res[0].Matched {
		if mlabel == "vec" {
			hasVec = true
		}
	}
	if !hasVec {
		t.Errorf("matched = %v, want vec to contribute", res[0].Matched)
	}
}

func TestEnsureVecTable_EmbedderSwitchErrors(t *testing.T) {
	m := newRAGStore(t)
	m.Embedder = &fakeEmbedder{name: "fake/a", def: []float32{1, 2, 3, 4}}
	ctx := context.Background()
	if _, err := m.IndexDocument(ctx, Document{Kind: "fact", Body: "first doc"}); err != nil {
		t.Fatalf("IndexDocument: %v", err)
	}
	// Same dim, different model name → must refuse to mix spaces.
	m.Embedder = &fakeEmbedder{name: "fake/b", def: []float32{4, 3, 2, 1}}
	_, err := m.IndexDocument(ctx, Document{Kind: "fact", Body: "second doc"})
	if err == nil || !strings.Contains(err.Error(), "reindex") {
		t.Fatalf("expected embedder-switch error mentioning reindex, got %v", err)
	}
}

func TestReindex_RebuildsFromFacts(t *testing.T) {
	m := newRAGStore(t)
	ctx := context.Background()

	// Facts written through the normal path are auto-indexed; Reindex
	// must produce the same searchable state from scratch.
	for _, body := range []string{"fact about quasars", "fact about black holes"} {
		if _, err := m.Write(Fact{Kind: "session_outcome", Body: body}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	indexed, embedded, err := m.Reindex(ctx)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if indexed != 2 || embedded != 0 {
		t.Errorf("Reindex = (%d, %d), want (2, 0) with no embedder", indexed, embedded)
	}
	res, err := m.SearchHybrid(ctx, "quasars", 5)
	if err != nil {
		t.Fatalf("SearchHybrid after reindex: %v", err)
	}
	if len(res) == 0 || !strings.Contains(res[0].Body, "quasars") {
		t.Errorf("post-reindex search failed: %+v", res)
	}
}

func TestWrite_AutoIndexesIntoRAG(t *testing.T) {
	m := newRAGStore(t)
	if _, err := m.Write(Fact{Kind: "lint_rule", Body: "never use tx.origin for auth checks"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, err := m.SearchHybrid(context.Background(), "tx.origin authorization", 3)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("fact written via Write was not searchable via RAG")
	}
}

func TestSearchHybrid_EmptyQueryAndNoDocs(t *testing.T) {
	m := newRAGStore(t)
	ctx := context.Background()
	if res, err := m.SearchHybrid(ctx, "", 5); err != nil || res != nil {
		t.Errorf("empty query: res=%v err=%v", res, err)
	}
	if res, err := m.SearchHybrid(ctx, "anything", 5); err != nil || len(res) != 0 {
		t.Errorf("empty index: res=%v err=%v", res, err)
	}
}

// FTS5 operator injection: a query containing AND/OR/quotes must not
// produce a syntax error — tokens are quoted before matching.
func TestSearchHybrid_FTSOperatorsNeutralized(t *testing.T) {
	m := newRAGStore(t)
	ctx := context.Background()
	if _, err := m.IndexDocument(ctx, Document{Kind: "fact", Body: "plain document"}); err != nil {
		t.Fatalf("IndexDocument: %v", err)
	}
	for _, q := range []string{`"unbalanced quote`, `a AND OR NOT b`, `col:value NEAR(x y)`} {
		if _, err := m.SearchHybrid(ctx, q, 3); err != nil {
			t.Errorf("query %q errored: %v", q, err)
		}
	}
}
