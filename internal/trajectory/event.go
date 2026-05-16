// Package trajectory carries the run-time event stream of an orchestration —
// every decomposition decision, subtask dispatch, worker chunk, error, and
// terminal state. The bus is a fan-out so future subscribers (Sentinel,
// budget guards, live TUI panes) plug in without modifying the producer.
package trajectory

import (
	"encoding/json"
	"time"
)

// Kind enumerates every trajectory event type. Closed enum: adding a value
// here is a deliberate change.
type Kind string

const (
	GoalReceived          Kind = "goal_received"
	PlanRequested         Kind = "plan_requested"
	PlanProposed          Kind = "plan_proposed"
	PlanFallback          Kind = "plan_fallback"
	SubtaskStarted        Kind = "subtask_started"
	SubtaskStdout         Kind = "subtask_stdout"
	SubtaskToolCall       Kind = "subtask_tool_call"
	SubtaskToolResult     Kind = "subtask_tool_result"
	SubtaskAssistantText  Kind = "subtask_assistant_text"
	SubtaskCompleted      Kind = "subtask_completed"
	SubtaskFailed         Kind = "subtask_failed"
	SynthesisStarted      Kind = "synthesis_started"
	SynthesisCompleted    Kind = "synthesis_completed"
	RunCompleted          Kind = "run_completed"
	RunFailed             Kind = "run_failed"
	RunCancelled          Kind = "run_cancelled"
)

// Event is one record on the trajectory.
type Event struct {
	SessionID string          `json:"session_id"`
	SubtaskID string          `json:"subtask_id,omitempty"`
	Seq       int64           `json:"seq"`
	Ts        time.Time       `json:"ts"`
	Kind      Kind            `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}
