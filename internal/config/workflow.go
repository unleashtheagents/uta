// Package config holds the loaders for user-authored YAML files: workflow
// definitions (`uta.yaml`) and declarative provider descriptors
// (`~/.uta/providers/*.yaml`). The loaders only parse and validate; they
// don't depend on the engine or registry.
package config

import (
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
	Defaults  WorkflowDefaults  `yaml:"defaults"`
	Synthesis WorkflowSynthesis `yaml:"synthesis"`
	Subtasks  []WorkflowSubtask `yaml:"subtasks"`
	Budget    WorkflowBudget    `yaml:"budget"`
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
	ID     string `yaml:"id"`
	Title  string `yaml:"title"`
	Prompt string `yaml:"prompt"`
	Worker string `yaml:"worker"`
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
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", path, err)
	}
	if err := wf.Validate(); err != nil {
		return nil, fmt.Errorf("workflow %s: %w", path, err)
	}
	return &wf, nil
}

func (w *Workflow) Validate() error {
	if w.Version != 0 && w.Version != 1 {
		return fmt.Errorf("unsupported workflow version %d (expected 1)", w.Version)
	}
	if strings.TrimSpace(w.Goal) == "" {
		return errors.New("goal is required")
	}
	if w.Defaults.Worker == "" {
		return errors.New("defaults.worker is required")
	}
	for i, st := range w.Subtasks {
		if strings.TrimSpace(st.Prompt) == "" {
			return fmt.Errorf("subtask %d (id=%q): prompt is required", i, st.ID)
		}
	}
	return nil
}

// ExampleWorkflowYAML is what `uta init --workflow` writes.
const ExampleWorkflowYAML = `version: 1

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
# as-is. Mix workers per subtask to dispatch the right shape of task to the
# right provider (e.g., gemini for whole-repo context, claude for delegation).
#
# subtasks:
#   - id: structure
#     title: "Top-level layout"
#     prompt: "List top-level dirs and one sentence on each."
#     worker: claude
#   - id: deps
#     title: "Dependencies summary"
#     prompt: "Read go.mod and summarize external deps."
#     worker: gemini

budget:
  max_wall_seconds: 1800   # placeholder — not enforced in v1
`
