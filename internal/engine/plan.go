package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SubtaskSpec is one decomposed unit of work.
type SubtaskSpec struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Prompt string   `json:"prompt"`
	Worker string   `json:"worker,omitempty"` // optional per-subtask override (YAML workflows)
	Needs  []string `json:"needs,omitempty"`  // DAG dependencies — subtask IDs that must complete first
	Gate   *Gate    `json:"gate,omitempty"`   // optional verification gate run after the subtask
}

// Gate is a verification step that runs after a subtask completes. A non-zero
// exit blocks downstream subtasks. When RetryProducer is true the producing
// subtask is re-run with the gate's output appended to its prompt, up to
// MaxRetries times.
type Gate struct {
	Cmd string `json:"cmd"`
	// Args, when non-empty, causes Cmd to be executed directly with these
	// arguments instead of being interpreted by `sh -c`. Direct execution
	// avoids shell-quoting hazards when the binary and arguments are known
	// up front (mirrors ToolSpec's non-ShellMode path).
	Args          []string      `json:"args,omitempty"`
	Timeout       time.Duration `json:"timeout,omitempty"`
	RetryProducer bool          `json:"retry_producer,omitempty"`
	MaxRetries    int           `json:"max_retries,omitempty"`
}

// Plan is what the planner LLM is asked to produce.
type Plan struct {
	Subtasks []SubtaskSpec `json:"subtasks"`
}

// plannerPrompt is rendered once per run; it instructs the chosen provider to
// return strict JSON suitable for json.Unmarshal'ing into a Plan.
const plannerPromptTmpl = `You are the planning step of an agent orchestrator called uta.

Your job: decompose the user's goal into between 1 and %d independent subtasks
that can run in parallel. Each subtask must be entirely self-contained — the
worker that executes it will NOT see other subtasks' outputs or the original
goal in any other way than what you write into its prompt.

Respond with ONLY a single JSON object, no prose, no markdown, no code fences.
The object MUST match this shape:

{
  "subtasks": [
    {"id": "s1", "title": "<short label>", "prompt": "<full instructions for the worker>"}
  ]
}

If the goal is small enough that decomposition would just add overhead, return
exactly one subtask that restates the goal.

Goal:
%s`

// RenderPlannerPrompt builds the prompt that goes to the planner provider.
func RenderPlannerPrompt(goal string, maxSubtasks int) string {
	if maxSubtasks <= 0 {
		maxSubtasks = 8
	}
	return fmt.Sprintf(plannerPromptTmpl, maxSubtasks, strings.TrimSpace(goal))
}

// ParsePlan extracts a Plan from a planner's free-form output. It's
// deliberately permissive: it locates the first '{' to the last '}' in the
// payload (so accidental prose around the JSON object is tolerated) and then
// json.Unmarshals. Returns an error wrapping ErrPlanUnparseable if no valid
// shape can be recovered.
func ParsePlan(text string) (Plan, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return Plan{}, fmt.Errorf("no JSON object found: %w", ErrPlanUnparseable)
	}
	candidate := text[start : end+1]
	var p Plan
	if err := json.Unmarshal([]byte(candidate), &p); err != nil {
		return Plan{}, fmt.Errorf("unmarshal plan: %w: %v", ErrPlanUnparseable, err)
	}
	if len(p.Subtasks) == 0 {
		return Plan{}, fmt.Errorf("plan has zero subtasks: %w", ErrPlanUnparseable)
	}
	// Ensure every subtask has at least an id + prompt; fill blanks defensively.
	for i := range p.Subtasks {
		if strings.TrimSpace(p.Subtasks[i].Prompt) == "" {
			return Plan{}, fmt.Errorf("subtask %d has empty prompt: %w", i, ErrPlanUnparseable)
		}
		if strings.TrimSpace(p.Subtasks[i].ID) == "" {
			p.Subtasks[i].ID = fmt.Sprintf("s%d", i+1)
		}
		if strings.TrimSpace(p.Subtasks[i].Title) == "" {
			p.Subtasks[i].Title = p.Subtasks[i].ID
		}
	}
	return p, nil
}

// FallbackPlan is what the supervisor uses when planning fails: a single
// subtask that just forwards the user's goal verbatim.
func FallbackPlan(goal string) Plan {
	return Plan{Subtasks: []SubtaskSpec{{
		ID:     "s1",
		Title:  "answer goal directly",
		Prompt: strings.TrimSpace(goal),
	}}}
}

// ErrPlanUnparseable signals the planner returned something we couldn't turn
// into a Plan. Callers fall back to FallbackPlan and emit plan_fallback.
var ErrPlanUnparseable = errors.New("plan unparseable")
