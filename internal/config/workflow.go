// Package config holds the loaders for user-authored YAML files: workflow
// definitions (`uta.yaml`) and declarative provider descriptors
// (`~/.uta/providers/*.yaml`). The loaders only parse and validate; they
// don't depend on the engine or registry.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Workflow is the on-disk shape of `uta.yaml`.
type Workflow struct {
	Version   int               `yaml:"version"`
	Goal      string            `yaml:"goal"`
	Strategy  string            `yaml:"strategy"` // "fanout" | "dag"; default auto-detected from subtasks
	Defaults  WorkflowDefaults  `yaml:"defaults"`
	Synthesis WorkflowSynthesis `yaml:"synthesis"`
	Subtasks  []WorkflowSubtask `yaml:"subtasks"`
	Budget    WorkflowBudget    `yaml:"budget"`
	// Env is a map of environment variables appended to os.Environ() for every
	// provider invocation in this run. Useful for project-specific config or
	// secrets sourced via shell expansion at YAML render time.
	Env map[string]string `yaml:"env"`
}

type WorkflowDefaults struct {
	Worker         string        `yaml:"worker"`
	Planner        string        `yaml:"planner"`
	MaxParallel    int           `yaml:"max_parallel"`
	MaxSubtasks    int           `yaml:"max_subtasks"`
	SubtaskTimeout time.Duration `yaml:"subtask_timeout"`
	Timeout        time.Duration `yaml:"timeout"`
	Workdir        string        `yaml:"workdir"`
	PreApprove     []string      `yaml:"pre_approve"`
}

type WorkflowSynthesis struct {
	Worker string `yaml:"worker"`
	Mode   string `yaml:"mode"`   // merge | skip
	Prompt string `yaml:"prompt"` // optional override; reserved for v2
}

type WorkflowSubtask struct {
	ID      string        `yaml:"id"`
	Title   string        `yaml:"title"`
	Prompt  string        `yaml:"prompt"`
	Worker  string        `yaml:"worker"`
	Needs   []string      `yaml:"needs"`
	Gate    *WorkflowGate `yaml:"gate"`
	Timeout time.Duration `yaml:"timeout"` // optional per-subtask override of defaults.subtask_timeout
}

// WorkflowGate is a verification step run after the subtask. Supports two
// YAML forms:
//
//	gate: forge build && forge test           # string shorthand
//
//	gate:
//	  cmd: forge test
//	  timeout: 5m
//	  retry_producer: true
//	  max_retries: 2
type WorkflowGate struct {
	Cmd           string        `yaml:"cmd"`
	Timeout       time.Duration `yaml:"timeout"`
	RetryProducer bool          `yaml:"retry_producer"`
	MaxRetries    int           `yaml:"max_retries"`
}

// UnmarshalYAML accepts either a plain string (shorthand for {cmd: <string>})
// or a mapping with the full field set.
func (g *WorkflowGate) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		g.Cmd = value.Value
		return nil
	}
	type raw WorkflowGate // avoid recursion into our UnmarshalYAML
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*g = WorkflowGate(r)
	return nil
}

type WorkflowBudget struct {
	MaxWallSeconds int `yaml:"max_wall_seconds"`
}

// LoadWorkflow reads a yaml file, validates its shape, and returns it.
func LoadWorkflow(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow %s: %w", path, err)
	}
	var wf Workflow
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&wf); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", path, err)
	}
	if err := wf.Validate(); err != nil {
		return nil, fmt.Errorf("workflow %s: %w", path, err)
	}
	return &wf, nil
}

func (w *Workflow) Validate() error {
	if w.Version != 0 && w.Version != 1 && w.Version != 2 {
		return fmt.Errorf("unsupported workflow version %d (expected 1 or 2)", w.Version)
	}
	if strings.TrimSpace(w.Goal) == "" {
		return errors.New("goal is required")
	}
	if w.Defaults.Worker == "" {
		return errors.New("defaults.worker is required")
	}
	switch w.Strategy {
	case "", "fanout", "dag":
	default:
		return fmt.Errorf("unsupported strategy %q (expected 'fanout' or 'dag')", w.Strategy)
	}
	ids := map[string]bool{}
	for i, st := range w.Subtasks {
		if strings.TrimSpace(st.Prompt) == "" {
			return fmt.Errorf("subtask %d (id=%q): prompt is required", i, st.ID)
		}
		if st.ID != "" {
			if !isValidSubtaskID(st.ID) {
				return fmt.Errorf("subtask %d: invalid id %q (allowed: A-Z a-z 0-9 . _ -, max 64)", i, st.ID)
			}
			if ids[st.ID] {
				return fmt.Errorf("subtask %d: duplicate id %q", i, st.ID)
			}
			ids[st.ID] = true
		}
		if st.Gate != nil && strings.TrimSpace(st.Gate.Cmd) == "" {
			return fmt.Errorf("subtask %d (id=%q): gate.cmd is required when gate is set", i, st.ID)
		}
		if st.Timeout < 0 {
			return fmt.Errorf("subtask %d (id=%q): timeout must be >= 0", i, st.ID)
		}
	}
	// Validate that every `needs` reference points at a real subtask id.
	for i, st := range w.Subtasks {
		for _, dep := range st.Needs {
			if !ids[dep] {
				return fmt.Errorf("subtask %d (id=%q): needs %q but no subtask with that id exists", i, st.ID, dep)
			}
		}
	}
	for k := range w.Env {
		if strings.TrimSpace(k) == "" {
			return errors.New("env: variable name must be non-empty")
		}
		if strings.ContainsRune(k, '=') {
			return fmt.Errorf("env: variable name %q must not contain '='", k)
		}
	}
	return nil
}

// isValidSubtaskID mirrors engine.isValidSubtaskID: subtask IDs flow into map
// keys, artifact filenames, JSON event payloads, and shell variable names, so
// we restrict them to a safe character set at load time.
func isValidSubtaskID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// InferStrategy returns the explicit Strategy field if set; otherwise it
// auto-detects: any subtask with needs or a gate triggers "dag", else
// "fanout".
func (w *Workflow) InferStrategy() string {
	if w.Strategy != "" {
		return w.Strategy
	}
	for _, st := range w.Subtasks {
		if len(st.Needs) > 0 || st.Gate != nil {
			return "dag"
		}
	}
	return "fanout"
}

// ExampleWorkflowYAML is what `uta init --workflow` writes.
const ExampleWorkflowYAML = `version: 2
# strategy: auto-detected from subtasks. Explicit values: 'fanout' (parallel,
# independent) or 'dag' (with needs + gate). If any subtask has 'needs' or
# 'gate' set, uta picks 'dag' automatically.

goal: |
  Summarize this repository in 5 concise bullets focused on architecture
  and the role of each top-level directory.

defaults:
  worker: claude          # the default provider for every subtask
  planner: claude         # who decomposes the goal (defaults to worker)
  max_parallel: 4         # max concurrent subtasks
  max_subtasks: 6         # hard cap on the planner's decomposition
  subtask_timeout: 10m
  timeout: 30m

synthesis:
  worker: claude          # who writes the final answer (defaults to worker)
  mode: merge             # 'merge' = synthesize; 'skip' = join subtask outputs verbatim

# subtasks (OPTIONAL): if present, the planner step is skipped and these run
# as-is. Mix workers per subtask; declare 'needs' for ordering; attach a
# verification 'gate' command that must exit 0 before downstream tasks run.
# Per-subtask 'timeout' overrides defaults.subtask_timeout — useful for
# mixed-workload DAGs (fast LLM call alongside a long-running security scan).
#
# subtasks:
#   - id: build
#     prompt: "Implement the contracts in ./contracts"
#     worker: claude
#     gate: forge build && forge test
#   - id: frontend
#     needs: [build]
#     prompt: "Build a Next.js app using the ABIs in ./contracts/out"
#     worker: claude
#     timeout: 30m            # this subtask gets 30 min; others use the default
#     gate:
#       cmd: npm run typecheck && npm run build
#       timeout: 10m
#       retry_producer: true
#       max_retries: 2

budget:
  max_wall_seconds: 1800   # hard wall-clock ceiling; cancels the run if exceeded

# env (OPTIONAL): KEY=VALUE pairs appended to the environment of every
# provider invocation. Useful for injecting project-specific config or
# secrets (sourced via shell expansion when this file is rendered).
#
# env:
#   API_BASE_URL: https://staging.example.com
#   FEATURE_FLAGS: experiment_a,experiment_b
`
