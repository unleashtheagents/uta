package workflows

import (
	"path/filepath"
	"testing"

	"github.com/unleashtheagents/uta/internal/eval"
)

// TestExampleEvalSuites_Load is the dry-run gate for the YAML suites
// under examples/evals/: every one must parse and validate, so a user
// who copies an example never starts from a broken file.
func TestExampleEvalSuites_Load(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "examples", "evals", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no example eval suites found — examples/evals/ should ship at least smoke.yaml")
	}
	for _, path := range matches {
		s, err := eval.LoadSuite(path)
		if err != nil {
			t.Errorf("LoadSuite(%s): %v", filepath.Base(path), err)
			continue
		}
		if len(s.Cases) == 0 {
			t.Errorf("%s: no cases", filepath.Base(path))
		}
	}
}
