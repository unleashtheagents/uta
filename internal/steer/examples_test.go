package steer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExamples_ParseAndCheck guards the shipped examples: every
// examples/*.steer must parse and check clean, so a construct change
// that breaks one fails here instead of in a user's shell.
func TestExamples_ParseAndCheck(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.steer"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no examples/*.steer found — did the examples directory move?")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			prog, d := Parse(filepath.Base(path), string(src))
			if d != nil {
				t.Fatalf("parse: %s", d.Render())
			}
			if diags := Check(prog, string(src)); HasErrors(diags) {
				t.Fatalf("check: %s", renderAll(diags))
			}
		})
	}
}
