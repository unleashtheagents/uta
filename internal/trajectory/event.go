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
	GoalReceived           Kind = "goal_received"
	PlanRequested          Kind = "plan_requested"
	PlanProposed           Kind = "plan_proposed"
	PlanFallback           Kind = "plan_fallback"
	SubtaskStarted         Kind = "subtask_started"
	SubtaskStdout          Kind = "subtask_stdout"
	SubtaskToolCall        Kind = "subtask_tool_call"
	SubtaskToolResult      Kind = "subtask_tool_result"
	SubtaskAssistantText   Kind = "subtask_assistant_text"
	SubtaskCompleted       Kind = "subtask_completed"
	SubtaskFailed          Kind = "subtask_failed"
	SubtaskSkipped         Kind = "subtask_skipped" // DAG: an upstream dep failed
	GateStarted            Kind = "gate_started"
	GatePassed             Kind = "gate_passed"
	GateFailed             Kind = "gate_failed"
	ProducerRetried        Kind = "producer_retried"
	IterationStarted       Kind = "iteration_started"
	IterationCompleted     Kind = "iteration_completed"
	CriticStarted          Kind = "critic_started"
	CriticCompleted        Kind = "critic_completed"
	ToolStarted            Kind = "tool_started"
	ToolCompleted          Kind = "tool_completed"
	ToolFailed             Kind = "tool_failed"
	FindingsAggregated     Kind = "findings_aggregated"
	ReviseStarted          Kind = "revise_started"
	ReviseCompleted        Kind = "revise_completed"
	ReflectorCompleted     Kind = "reflector_completed"
	SentinelAlert          Kind = "sentinel_alert"
	CapabilityGateDenied   Kind = "capability_gate_denied"
	BudgetWarning          Kind = "budget_warning"
	BudgetExhausted        Kind = "budget_exhausted"
	HITLRequested          Kind = "hitl_requested"
	HITLApproved           Kind = "hitl_approved"
	HITLDenied             Kind = "hitl_denied"
	SynthesisStarted       Kind = "synthesis_started"
	SynthesisCompleted     Kind = "synthesis_completed"
	HandoffStarted         Kind = "handoff_started"
	HandoffCompleted       Kind = "handoff_completed"
	HandoffCancelled       Kind = "handoff_cancelled"
	HandoffSkipped         Kind = "handoff_skipped"
	MemoryFactWritten      Kind = "memory_fact_written"
	MemoryFactsInjected    Kind = "memory_facts_injected"
	WhiteboardSet          Kind = "whiteboard_set"
	WhiteboardGet          Kind = "whiteboard_get"
	RetrospectiveStarted   Kind = "retrospective_started"
	RetrospectiveCompleted Kind = "retrospective_completed"
	DiscussionTurnStarted  Kind = "discussion_turn_started"
	DiscussionTurnDone     Kind = "discussion_turn_completed"
	DiscussionCompleted    Kind = "discussion_completed"
	RunCompleted           Kind = "run_completed"
	RunFailed              Kind = "run_failed"
	RunCancelled           Kind = "run_cancelled"
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
