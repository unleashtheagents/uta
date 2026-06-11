// Package eval is uta's regression harness for orchestration quality.
// A suite is a YAML file of golden goals with assertions; the runner
// executes each case (optionally across a worker matrix) through the
// real supervisor and grades the outcome. Use it to answer "did my
// profile / prompt / provider change make things worse?" before
// trusting the change.
//
// The package is engine-agnostic at the seam: the runner takes a
// RunFunc so tests grade assertion logic without spawning providers,
// and the CLI wires the real supervisor in.
package eval

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Suite is the on-disk shape of an eval file.
type Suite struct {
	Version int    `yaml:"version"`
	Name    string `yaml:"name"`
	// Workers is the default provider matrix — every case runs once per
	// worker unless the case pins its own. Empty means "whatever the
	// CLI's --worker resolution picks".
	Workers []string `yaml:"workers"`
	// Defaults applied to every case that doesn't override.
	Defaults CaseDefaults `yaml:"defaults"`
	Cases    []Case       `yaml:"cases"`
}

// CaseDefaults holds suite-wide knobs.
type CaseDefaults struct {
	Timeout     time.Duration `yaml:"timeout"`      // per-case wall clock
	MaxSubtasks int           `yaml:"max_subtasks"` // planner cap
	Mode        string        `yaml:"mode"`         // MissionProfile name
	Workdir     string        `yaml:"workdir"`
}

// Case is one golden goal.
type Case struct {
	Name    string        `yaml:"name"`
	Goal    string        `yaml:"goal"`
	Workers []string      `yaml:"workers"` // overrides suite matrix
	Mode    string        `yaml:"mode"`
	Workdir string        `yaml:"workdir"`
	Timeout time.Duration `yaml:"timeout"`
	Assert  Assertions    `yaml:"assert"`
}

// Assertions grade one case result. All configured checks must pass.
type Assertions struct {
	// Contains: every entry must appear in the final answer
	// (case-insensitive).
	Contains []string `yaml:"contains"`
	// NotContains: no entry may appear (case-insensitive).
	NotContains []string `yaml:"not_contains"`
	// Gate: shell command run after the case (in the case workdir);
	// exit 0 = pass. The final answer is piped to stdin and exported
	// as $UTA_EVAL_ANSWER.
	Gate string `yaml:"gate"`
	// MinChars guards against empty/truncated answers.
	MinChars int `yaml:"min_chars"`
	// MaxUSDCents fails the case when the run cost more.
	// 0 = unchecked.
	MaxUSDCents int64 `yaml:"max_usd_cents"`
	// MaxTokens fails the case when tokens_in+tokens_out exceeded it.
	// 0 = unchecked.
	MaxTokens int64 `yaml:"max_tokens"`
	// Status, when set, requires the session to end in this exact
	// status (default: "completed").
	Status string `yaml:"status"`
}

// LoadSuite reads + validates an eval suite. Validation is strict —
// a typoed assertion key failing silently would make the whole harness
// lie, so unknown fields are rejected.
func LoadSuite(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read suite: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var s Suite
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("eval: %s: %w", path, err)
	}
	return &s, nil
}

func (s *Suite) validate() error {
	if len(s.Cases) == 0 {
		return errors.New("suite has no cases")
	}
	seen := map[string]bool{}
	for i := range s.Cases {
		c := &s.Cases[i]
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("case %d: name is required", i+1)
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.Goal) == "" {
			return fmt.Errorf("case %q: goal is required", c.Name)
		}
		a := c.Assert
		if len(a.Contains) == 0 && len(a.NotContains) == 0 && a.Gate == "" &&
			a.MinChars == 0 && a.MaxUSDCents == 0 && a.MaxTokens == 0 && a.Status == "" {
			return fmt.Errorf("case %q: at least one assertion is required (a case that can't fail isn't an eval)", c.Name)
		}
	}
	return nil
}

// matrixFor resolves the worker list for one case: case-level pin wins,
// then the suite matrix, then the single fallback the caller supplies.
func (s *Suite) matrixFor(c *Case, fallback string) []string {
	if len(c.Workers) > 0 {
		return c.Workers
	}
	if len(s.Workers) > 0 {
		return s.Workers
	}
	if fallback != "" {
		return []string{fallback}
	}
	return []string{""}
}
