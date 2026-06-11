// Package profileperf turns recorded trajectory events into post-hoc latency
// breakdowns. The supervisor already stamps every event with a monotonic
// wall-clock timestamp; this package pairs the start/end events that bracket
// each phase (planner / subtask / synthesis / gate / external tool / provider
// tool call) and reports the deltas as durations.
//
// Nothing in here writes to the trajectory or instruments any new code path —
// it operates strictly on the recorded event stream so old sessions remain
// analyzable.
package profileperf

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// Tool kinds. MCP tools are conventionally named "mcp__<server>__<tool>"
// by claude's MCP bridge; everything else (Bash, Read, Edit, Grep, ...)
// is treated as a provider-internal builtin.
const (
	ToolKindBuiltin = "builtin"
	ToolKindMCP     = "mcp"
)

// ClassifyTool returns ToolKindMCP for names that match claude's MCP
// bridge namespacing ("mcp__server__tool") and ToolKindBuiltin otherwise.
// Exported so callers can label tool names from outside the breakdown
// (e.g. dashboards) using the same convention.
func ClassifyTool(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		return ToolKindMCP
	}
	return ToolKindBuiltin
}

// Breakdown is the per-session latency picture. Wall-clock is the
// span between the first and last recorded event; CPUEquivalent is the
// non-parallel sum of all measured phases (parallel subtasks therefore
// inflate CPUEquivalent above WallClock, which the renderer surfaces as a
// "parallel speedup" ratio).
type Breakdown struct {
	SessionID         string
	Goal              string
	Status            string
	Worker            string
	ModeName          string
	Start             time.Time
	End               time.Time
	WallClock         time.Duration
	Planner           Phase
	Subtasks          []SubtaskPhase
	SubtasksTotal     time.Duration
	Synthesis         Phase
	Gates             []GatePhase
	GatesTotal        time.Duration
	ExternalTools     []ToolPhase
	ToolsTotal        time.Duration
	ProviderTools     []ToolUsage
	ProviderToolTotal time.Duration
	// BuiltinToolTotal / MCPToolTotal split ProviderToolTotal by tool kind
	// so the renderer can show "how much of the tool budget went to MCP
	// servers" — useful for ops-mode runs where MCP calls dominate.
	BuiltinToolTotal time.Duration
	MCPToolTotal     time.Duration
	SentinelAlerts   int
	CPUEquivalent    time.Duration
}

// Phase is the simplest shape — one duration, plus how many times it
// fired in this session (planner can fire once normally; synthesis fires
// once or zero times).
type Phase struct {
	Duration time.Duration
	Count    int
}

// SubtaskPhase is one provider-backed subtask. Status reflects the
// terminal event (subtask_completed → "completed", subtask_failed →
// "failed", neither → "running").
type SubtaskPhase struct {
	SubtaskID string
	SpecID    string
	Title     string
	Worker    string
	Status    string
	Duration  time.Duration
}

// GatePhase is one gate execution (gate_started → gate_passed|gate_failed).
type GatePhase struct {
	SubtaskID string
	Cmd       string
	Status    string // "passed" | "failed"
	Duration  time.Duration
}

// ToolPhase is one external tool-adapter invocation (the reflector loop's
// tool_started → tool_completed|tool_failed pair).
type ToolPhase struct {
	SubtaskID string
	Tool      string
	Status    string // "completed" | "failed"
	Duration  time.Duration
}

// ToolUsage aggregates provider-level tool calls (Read, Bash, Edit, ...)
// across all subtasks of a session. Duration is the FIFO-paired sum of
// each subtask_tool_call → next subtask_tool_result within the same
// subtask. The pairing is a heuristic — providers don't carry a usable
// id back on the result event — so the totals are approximate when calls
// interleave. Kind is the ClassifyTool bucket ("builtin" or "mcp").
type ToolUsage struct {
	Tool     string
	Kind     string
	Count    int
	Duration time.Duration
}

// BuildSessionBreakdown derives a Breakdown from the persisted session row
// + its ordered event stream. Events must arrive in seq order; the store's
// ListEvents already returns them that way.
func BuildSessionBreakdown(sess store.Session, events []store.EventRow) Breakdown {
	b := Breakdown{
		SessionID: sess.ID,
		Goal:      sess.Goal,
		Status:    sess.Status,
		Worker:    sess.Worker,
		ModeName:  sess.ModeName,
	}
	if len(events) == 0 {
		return b
	}
	b.Start = events[0].Ts
	b.End = events[len(events)-1].Ts
	b.WallClock = b.End.Sub(b.Start)

	// Per-phase start times keyed by subtask id (or "" for run-level).
	var plannerStart time.Time
	subStart := map[string]time.Time{}
	subTitle := map[string]string{}
	subSpec := map[string]string{}
	subWorker := map[string]string{}
	gateStart := map[string]time.Time{}
	gateCmd := map[string]string{}
	toolStart := map[string]time.Time{}
	toolName := map[string]string{}
	var synthStart time.Time

	// Provider tool-call FIFO queues, per subtask. Each queue holds (name,
	// startedAt) of unmatched subtask_tool_call events; the next
	// subtask_tool_result on that subtask pops the head.
	type pendingCall struct {
		Name      string
		StartedAt time.Time
	}
	pending := map[string][]pendingCall{}

	subStatus := map[string]string{}

	for _, ev := range events {
		switch trajectory.Kind(ev.Kind) {
		case trajectory.PlanRequested:
			plannerStart = ev.Ts
		case trajectory.PlanProposed, trajectory.PlanFallback:
			if !plannerStart.IsZero() {
				b.Planner.Duration += ev.Ts.Sub(plannerStart)
				b.Planner.Count++
				plannerStart = time.Time{}
			}

		case trajectory.SubtaskStarted:
			subStart[ev.SubtaskID] = ev.Ts
			subStatus[ev.SubtaskID] = "running"
			p := decodePayload(ev.Payload)
			if v, ok := p["title"].(string); ok {
				subTitle[ev.SubtaskID] = v
			}
			if v, ok := p["spec_id"].(string); ok {
				subSpec[ev.SubtaskID] = v
			}
			if v, ok := p["worker"].(string); ok {
				subWorker[ev.SubtaskID] = v
			}
		case trajectory.SubtaskCompleted:
			closeSubtask(&b, ev, subStart, subTitle, subSpec, subWorker, subStatus, "completed")
		case trajectory.SubtaskFailed:
			closeSubtask(&b, ev, subStart, subTitle, subSpec, subWorker, subStatus, "failed")

		case trajectory.GateStarted:
			gateStart[ev.SubtaskID] = ev.Ts
			p := decodePayload(ev.Payload)
			if v, ok := p["cmd"].(string); ok {
				gateCmd[ev.SubtaskID] = v
			}
		case trajectory.GatePassed:
			closeGate(&b, ev, gateStart, gateCmd, "passed")
		case trajectory.GateFailed:
			closeGate(&b, ev, gateStart, gateCmd, "failed")

		case trajectory.ToolStarted:
			toolStart[ev.SubtaskID] = ev.Ts
			p := decodePayload(ev.Payload)
			if v, ok := p["tool"].(string); ok {
				toolName[ev.SubtaskID] = v
			}
		case trajectory.ToolCompleted:
			closeTool(&b, ev, toolStart, toolName, "completed")
		case trajectory.ToolFailed:
			closeTool(&b, ev, toolStart, toolName, "failed")

		case trajectory.SynthesisStarted:
			synthStart = ev.Ts
		case trajectory.SynthesisCompleted:
			if !synthStart.IsZero() {
				b.Synthesis.Duration += ev.Ts.Sub(synthStart)
				b.Synthesis.Count++
				synthStart = time.Time{}
			} else {
				// "skip" path: the engine emits SynthesisCompleted alone with
				// mode=skip. Count it but assign zero duration.
				p := decodePayload(ev.Payload)
				if v, ok := p["mode"].(string); ok && v == "skip" {
					b.Synthesis.Count++
				}
			}

		case trajectory.SubtaskToolCall:
			p := decodePayload(ev.Payload)
			name := stringOr(p, "name", "tool", "tool_name")
			if name == "" {
				name = "(unknown)"
			}
			pending[ev.SubtaskID] = append(pending[ev.SubtaskID], pendingCall{Name: name, StartedAt: ev.Ts})
		case trajectory.SubtaskToolResult:
			q := pending[ev.SubtaskID]
			if len(q) == 0 {
				continue
			}
			head := q[0]
			pending[ev.SubtaskID] = q[1:]
			addToolUsage(&b, head.Name, ev.Ts.Sub(head.StartedAt))

		case trajectory.SentinelAlert:
			b.SentinelAlerts++
		}
	}

	// Any subtasks still running at the last event get assigned the
	// (end - start) span so the wall-clock view stays accurate.
	for id, start := range subStart {
		b.Subtasks = append(b.Subtasks, SubtaskPhase{
			SubtaskID: id,
			SpecID:    subSpec[id],
			Title:     subTitle[id],
			Worker:    subWorker[id],
			Status:    subStatus[id],
			Duration:  b.End.Sub(start),
		})
		b.SubtasksTotal += b.End.Sub(start)
	}

	sortSubtasks(b.Subtasks)

	b.CPUEquivalent = b.Planner.Duration + b.SubtasksTotal + b.Synthesis.Duration + b.GatesTotal + b.ToolsTotal

	// Per-tool list sorted by total duration desc.
	sort.SliceStable(b.ProviderTools, func(i, j int) bool {
		if b.ProviderTools[i].Duration == b.ProviderTools[j].Duration {
			return b.ProviderTools[i].Tool < b.ProviderTools[j].Tool
		}
		return b.ProviderTools[i].Duration > b.ProviderTools[j].Duration
	})

	return b
}

func closeSubtask(b *Breakdown, ev store.EventRow, subStart map[string]time.Time, subTitle, subSpec, subWorker, subStatus map[string]string, status string) {
	start, ok := subStart[ev.SubtaskID]
	if !ok {
		return
	}
	dur := ev.Ts.Sub(start)
	b.Subtasks = append(b.Subtasks, SubtaskPhase{
		SubtaskID: ev.SubtaskID,
		SpecID:    subSpec[ev.SubtaskID],
		Title:     subTitle[ev.SubtaskID],
		Worker:    subWorker[ev.SubtaskID],
		Status:    status,
		Duration:  dur,
	})
	b.SubtasksTotal += dur
	delete(subStart, ev.SubtaskID)
	delete(subTitle, ev.SubtaskID)
	delete(subSpec, ev.SubtaskID)
	delete(subWorker, ev.SubtaskID)
	subStatus[ev.SubtaskID] = status
}

func closeGate(b *Breakdown, ev store.EventRow, gateStart map[string]time.Time, gateCmd map[string]string, status string) {
	start, ok := gateStart[ev.SubtaskID]
	if !ok {
		return
	}
	dur := ev.Ts.Sub(start)
	b.Gates = append(b.Gates, GatePhase{
		SubtaskID: ev.SubtaskID,
		Cmd:       gateCmd[ev.SubtaskID],
		Status:    status,
		Duration:  dur,
	})
	b.GatesTotal += dur
	delete(gateStart, ev.SubtaskID)
	delete(gateCmd, ev.SubtaskID)
}

func closeTool(b *Breakdown, ev store.EventRow, toolStart map[string]time.Time, toolName map[string]string, status string) {
	start, ok := toolStart[ev.SubtaskID]
	if !ok {
		return
	}
	dur := ev.Ts.Sub(start)
	b.ExternalTools = append(b.ExternalTools, ToolPhase{
		SubtaskID: ev.SubtaskID,
		Tool:      toolName[ev.SubtaskID],
		Status:    status,
		Duration:  dur,
	})
	b.ToolsTotal += dur
	delete(toolStart, ev.SubtaskID)
	delete(toolName, ev.SubtaskID)
}

func addToolUsage(b *Breakdown, name string, dur time.Duration) {
	kind := ClassifyTool(name)
	for i := range b.ProviderTools {
		if b.ProviderTools[i].Tool == name {
			b.ProviderTools[i].Count++
			b.ProviderTools[i].Duration += dur
			b.ProviderToolTotal += dur
			b.addKindTotal(kind, dur)
			return
		}
	}
	b.ProviderTools = append(b.ProviderTools, ToolUsage{Tool: name, Kind: kind, Count: 1, Duration: dur})
	b.ProviderToolTotal += dur
	b.addKindTotal(kind, dur)
}

func (b *Breakdown) addKindTotal(kind string, dur time.Duration) {
	switch kind {
	case ToolKindMCP:
		b.MCPToolTotal += dur
	default:
		b.BuiltinToolTotal += dur
	}
}

func sortSubtasks(ss []SubtaskPhase) {
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].SpecID != "" && ss[j].SpecID != "" {
			return ss[i].SpecID < ss[j].SpecID
		}
		return ss[i].SubtaskID < ss[j].SubtaskID
	})
}

func decodePayload(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func stringOr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// Aggregate is the across-session view used by `uta perf --mode <name>
// --since <duration>`. All time fields are simple sums (planner time
// across N sessions, etc.); top-tool list is the merged per-tool view.
type Aggregate struct {
	Sessions          int
	WallClock         time.Duration
	PlannerTime       time.Duration
	SubtasksTime      time.Duration
	SynthesisTime     time.Duration
	GatesTime         time.Duration
	ExternalToolsTime time.Duration
	ProviderToolTime  time.Duration
	BuiltinToolTime   time.Duration
	MCPToolTime       time.Duration
	SentinelAlerts    int
	CPUEquivalent     time.Duration
	TopTools          []ToolUsage
}

// BuildAggregate folds N per-session breakdowns into one cross-cut view.
// TopTools is sorted by total duration descending.
func BuildAggregate(items []Breakdown) Aggregate {
	agg := Aggregate{Sessions: len(items)}
	merged := map[string]*ToolUsage{}
	for _, b := range items {
		agg.WallClock += b.WallClock
		agg.PlannerTime += b.Planner.Duration
		agg.SubtasksTime += b.SubtasksTotal
		agg.SynthesisTime += b.Synthesis.Duration
		agg.GatesTime += b.GatesTotal
		agg.ExternalToolsTime += b.ToolsTotal
		agg.ProviderToolTime += b.ProviderToolTotal
		agg.BuiltinToolTime += b.BuiltinToolTotal
		agg.MCPToolTime += b.MCPToolTotal
		agg.SentinelAlerts += b.SentinelAlerts
		agg.CPUEquivalent += b.CPUEquivalent
		for _, t := range b.ProviderTools {
			cur, ok := merged[t.Tool]
			if !ok {
				cur = &ToolUsage{Tool: t.Tool, Kind: t.Kind}
				merged[t.Tool] = cur
			}
			cur.Count += t.Count
			cur.Duration += t.Duration
		}
	}
	for _, t := range merged {
		agg.TopTools = append(agg.TopTools, *t)
	}
	sort.SliceStable(agg.TopTools, func(i, j int) bool {
		if agg.TopTools[i].Duration == agg.TopTools[j].Duration {
			return agg.TopTools[i].Tool < agg.TopTools[j].Tool
		}
		return agg.TopTools[i].Duration > agg.TopTools[j].Duration
	})
	return agg
}
