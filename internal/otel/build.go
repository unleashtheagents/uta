package otel

import (
	"encoding/json"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

// BuildTrace converts one uta session (row + subtasks + trajectory
// events) into an OTLP trace:
//
//	invoke_agent uta            <- root span, whole session
//	├─ plan                     <- plan_requested .. plan_proposed
//	├─ subtask <title>          <- one span per subtask row
//	│    events: tool calls, gate outcomes
//	├─ ...
//	└─ synthesis                <- synthesis_started .. synthesis_completed
//
// Attribute names follow the OTel GenAI semantic conventions where one
// exists (gen_ai.operation.name, gen_ai.agent.name,
// gen_ai.usage.input_tokens / output_tokens); uta-specific data uses
// the uta.* namespace.
func BuildTrace(sess store.Session, subtasks []store.Subtask, events []store.EventRow) ExportTraceServiceRequest {
	traceID := TraceIDFromSession(sess.ID)
	rootSpanID := SpanIDFrom("session:" + sess.ID)

	start, end := sessionWindow(sess, events)

	root := Span{
		TraceID:           traceID,
		SpanID:            rootSpanID,
		Name:              "invoke_agent uta",
		Kind:              SpanKindInternal,
		StartTimeUnixNano: nanos(start),
		EndTimeUnixNano:   nanos(end),
		Attributes: []KeyValue{
			Str("gen_ai.operation.name", "invoke_agent"),
			Str("gen_ai.agent.name", "uta"),
			Str("uta.session.id", sess.ID),
			Str("uta.session.goal", sess.Goal),
			Str("uta.session.status", sess.Status),
			Str("uta.worker", sess.Worker),
		},
		Status: statusFor(sess.Status),
	}
	if sess.ModeName != "" {
		root.Attributes = append(root.Attributes, Str("uta.mode", sess.ModeName))
	}

	spans := []Span{root}
	spans = append(spans, phaseSpans(sess, events, traceID, rootSpanID)...)
	spans = append(spans, subtaskSpans(sess, subtasks, events, traceID, rootSpanID)...)

	return ExportTraceServiceRequest{
		ResourceSpans: []ResourceSpans{{
			Resource: Resource{Attributes: []KeyValue{
				Str("service.name", "uta"),
				Str("service.version", version.Version),
			}},
			ScopeSpans: []ScopeSpans{{
				Scope: Scope{Name: "github.com/unleashtheagents/uta", Version: version.Version},
				Spans: spans,
			}},
		}},
	}
}

// sessionWindow is [first event ts, last event ts]; falls back to the
// session row's timestamps when the trajectory is empty.
func sessionWindow(sess store.Session, events []store.EventRow) (time.Time, time.Time) {
	if len(events) > 0 {
		return events[0].Ts, events[len(events)-1].Ts
	}
	start := sess.CreatedAt
	end := start
	if sess.CompletedAt != nil {
		end = *sess.CompletedAt
	}
	return start, end
}

// phaseSpans extracts the planner and synthesis windows from the event
// stream. Either may be absent (preset subtasks skip planning;
// SkipSynthesis runs have no synthesis window).
func phaseSpans(sess store.Session, events []store.EventRow, traceID, rootSpanID string) []Span {
	var out []Span
	if span, ok := windowSpan(events, "plan_requested", "plan_proposed", "plan",
		sess.ID, traceID, rootSpanID); ok {
		span.Attributes = append(span.Attributes, Str("gen_ai.operation.name", "plan"))
		out = append(out, span)
	}
	if span, ok := windowSpan(events, "synthesis_started", "synthesis_completed", "synthesis",
		sess.ID, traceID, rootSpanID); ok {
		span.Attributes = append(span.Attributes, Str("gen_ai.operation.name", "synthesize"))
		out = append(out, span)
	}
	return out
}

// windowSpan builds a span covering the first startKind .. first
// endKind-after-it pair. ok=false when either edge is missing.
func windowSpan(events []store.EventRow, startKind, endKind, name, sessionID, traceID, parent string) (Span, bool) {
	var start, end *time.Time
	for i := range events {
		if events[i].Kind == startKind && start == nil {
			start = &events[i].Ts
			continue
		}
		if events[i].Kind == endKind && start != nil {
			end = &events[i].Ts
			break
		}
	}
	if start == nil || end == nil {
		return Span{}, false
	}
	return Span{
		TraceID:           traceID,
		SpanID:            SpanIDFrom(name + ":" + sessionID),
		ParentSpanID:      parent,
		Name:              name,
		Kind:              SpanKindInternal,
		StartTimeUnixNano: nanos(*start),
		EndTimeUnixNano:   nanos(*end),
		Status:            SpanStatus{Code: StatusOK},
	}, true
}

// subtaskSpans renders one span per subtask row, with tool calls and
// gate outcomes from the trajectory attached as span events.
func subtaskSpans(sess store.Session, subtasks []store.Subtask, events []store.EventRow, traceID, rootSpanID string) []Span {
	// Group point events by subtask so each span's annotations attach
	// to the right place.
	type pointEvent struct {
		ts   time.Time
		kind string
	}
	pointsBySubtask := map[string][]pointEvent{}
	for _, ev := range events {
		switch ev.Kind {
		case "subtask_tool_call", "subtask_tool_result", "gate_started", "gate_passed",
			"gate_failed", "capability_gate_denied", "budget_warning",
			"hitl_requested", "hitl_approved", "hitl_denied":
			if ev.SubtaskID != "" {
				pointsBySubtask[ev.SubtaskID] = append(pointsBySubtask[ev.SubtaskID],
					pointEvent{ts: ev.Ts, kind: ev.Kind})
			}
		}
	}

	var out []Span
	for _, st := range subtasks {
		if st.StartedAt == nil {
			continue // never dispatched (skipped DAG nodes)
		}
		end := st.StartedAt
		if st.CompletedAt != nil {
			end = st.CompletedAt
		}
		span := Span{
			TraceID:           traceID,
			SpanID:            SpanIDFrom("subtask:" + st.ID),
			ParentSpanID:      rootSpanID,
			Name:              "subtask " + st.Title,
			Kind:              SpanKindInternal,
			StartTimeUnixNano: nanos(*st.StartedAt),
			EndTimeUnixNano:   nanos(*end),
			Attributes: []KeyValue{
				Str("gen_ai.operation.name", "execute_task"),
				Str("gen_ai.agent.name", st.Worker),
				Str("uta.subtask.id", st.ID),
				Str("uta.subtask.status", st.Status),
			},
			Status: statusFor(st.Status),
		}
		if st.Error != "" {
			span.Status.Message = st.Error
			span.Attributes = append(span.Attributes, Str("uta.subtask.error_kind", st.ErrorKind))
		}
		if in, outTok, cents, ok := usageFromMeta(st.MetaJSON); ok {
			span.Attributes = append(span.Attributes,
				Int("gen_ai.usage.input_tokens", in),
				Int("gen_ai.usage.output_tokens", outTok),
				Int("uta.usage.usd_cents", cents),
			)
		}
		for _, p := range pointsBySubtask[st.ID] {
			span.Events = append(span.Events, SpanEvent{
				TimeUnixNano: nanos(p.ts),
				Name:         p.kind,
			})
		}
		out = append(out, span)
	}
	return out
}

// usageFromMeta reads the tokens_in/tokens_out/usd_cents triplet the
// supervisor stores in subtask meta_json. ok=false for rows from before
// usage persistence (meta "{}").
func usageFromMeta(meta string) (in, out, cents int64, ok bool) {
	if meta == "" || meta == "{}" {
		return 0, 0, 0, false
	}
	var m struct {
		In    *int64 `json:"tokens_in"`
		Out   *int64 `json:"tokens_out"`
		Cents *int64 `json:"usd_cents"`
	}
	if err := json.Unmarshal([]byte(meta), &m); err != nil || m.In == nil {
		return 0, 0, 0, false
	}
	in = *m.In
	if m.Out != nil {
		out = *m.Out
	}
	if m.Cents != nil {
		cents = *m.Cents
	}
	return in, out, cents, true
}

// statusFor maps uta's status strings onto OTLP status codes. Unknown
// strings stay UNSET rather than guessing.
func statusFor(status string) SpanStatus {
	switch status {
	case "completed", "done", "ok":
		return SpanStatus{Code: StatusOK}
	case "failed", "cancelled", "budget_exhausted":
		return SpanStatus{Code: StatusError, Message: status}
	default:
		return SpanStatus{Code: StatusUnset}
	}
}

func nanos(t time.Time) string {
	return itoa64(t.UnixNano())
}
