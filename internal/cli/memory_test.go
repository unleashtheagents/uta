package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/memory"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/store"
)

// setupMemoryProject mirrors setupIdeasProject: isolated HOME, isolated UTA
// global home, and an initialized project rooted at a fresh temp dir. Tests
// chdir into the project so newApp() resolves to the project DB.
func setupMemoryProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("UTA_HOME", "")

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := config.SaveProject(root, &config.Project{Name: "memory-test"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return root
}

// runMemory builds a fresh memory command tree and executes it with the
// given args. Returns stdout, stderr, and the executor error.
func runMemory(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newMemoryCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// seedMemoryFacts opens the project DB directly and writes the supplied
// facts, then closes the store so the CLI can reopen it.
func seedMemoryFacts(t *testing.T, root string, facts ...memory.Fact) {
	t.Helper()
	st, err := store.Open(paths.DB(paths.ProjectStateDir(root)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	m := memory.New(st.DB)
	for _, f := range facts {
		if _, err := m.Write(f); err != nil {
			t.Fatalf("seed Write: %v", err)
		}
	}
}

func TestMemoryStats_TextEmpty(t *testing.T) {
	setupMemoryProject(t)

	out, _, err := runMemory(t, "stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	// Header and TOTAL row should render even when there are no rows.
	if !strings.Contains(out, "SOURCE_MODE") || !strings.Contains(out, "COUNT") {
		t.Errorf("missing header row: %q", out)
	}
	if !strings.Contains(out, "TOTAL") || !strings.Contains(out, "0") {
		t.Errorf("missing TOTAL row with 0: %q", out)
	}
}

func TestMemoryStats_TextAggregatesByMode(t *testing.T) {
	root := setupMemoryProject(t)

	// Seed: 2 audit + 1 dev + 1 with empty source_mode. The CLI groups by
	// source_mode and should render the empty mode as "(none)".
	seedMemoryFacts(t, root,
		memory.Fact{SourceMode: "audit", Kind: "lint_rule", Body: "b1", Tags: []string{"alpha"}},
		memory.Fact{SourceMode: "audit", Kind: "lint_rule", Body: "b2", Tags: []string{"beta"}},
		memory.Fact{SourceMode: "dev", Kind: "note", Body: "b3", Tags: []string{"gamma"}},
		memory.Fact{SourceMode: "", Kind: "note", Body: "b4", Tags: []string{"delta"}},
	)

	out, _, err := runMemory(t, "stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	// Each mode label should appear with its count on the same line.
	wantLine := func(label string, count int) {
		t.Helper()
		found := false
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if fields[0] == label && fields[len(fields)-1] == itoa(count) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected line with label=%q count=%d in:\n%s", label, count, out)
		}
	}

	wantLine("audit", 2)
	wantLine("dev", 1)
	// Graceful handling of empty source_mode: rendered as "(none)".
	wantLine("(none)", 1)
	wantLine("TOTAL", 4)
}

func TestMemoryStats_JSONEmission(t *testing.T) {
	root := setupMemoryProject(t)

	seedMemoryFacts(t, root,
		memory.Fact{SourceMode: "audit", Kind: "lint_rule", Body: "b1", Tags: []string{"alpha"}},
		memory.Fact{SourceMode: "dev", Kind: "note", Body: "b2", Tags: []string{"beta"}},
		memory.Fact{SourceMode: "dev", Kind: "note", Body: "b3", Tags: []string{"gamma"}},
		memory.Fact{SourceMode: "", Kind: "note", Body: "b4", Tags: []string{"delta"}},
	)

	out, _, err := runMemory(t, "stats", "--json")
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}

	var payload struct {
		Total        int            `json:"total"`
		BySourceMode map[string]int `json:"by_source_mode"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}

	if payload.Total != 4 {
		t.Errorf("total: got %d want 4", payload.Total)
	}
	want := map[string]int{"audit": 1, "dev": 2, "": 1}
	if len(payload.BySourceMode) != len(want) {
		t.Errorf("by_source_mode size: got %d want %d (%v)", len(payload.BySourceMode), len(want), payload.BySourceMode)
	}
	for k, v := range want {
		if payload.BySourceMode[k] != v {
			t.Errorf("by_source_mode[%q]: got %d want %d", k, payload.BySourceMode[k], v)
		}
	}
}

func TestMemoryStats_JSONFlagParsing(t *testing.T) {
	// With no facts and --json, output must still be valid JSON (not the
	// tab-writer text format). This pins down the flag parsing path.
	setupMemoryProject(t)

	out, _, err := runMemory(t, "stats", "--json")
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("empty --json should be valid JSON: %v\nraw=%s", err, out)
	}
	if _, ok := payload["total"]; !ok {
		t.Errorf("json missing 'total' key: %v", payload)
	}
	if _, ok := payload["by_source_mode"]; !ok {
		t.Errorf("json missing 'by_source_mode' key: %v", payload)
	}
}

// itoa is a tiny helper that keeps the text-mode assertions readable
// without pulling in strconv at the call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
