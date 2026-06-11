package otel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

func ts(sec int64) time.Time { return time.Unix(1_700_000_000+sec, 0).UTC() }

func sampleSession() (store.Session, []store.Subtask, []store.EventRow) {
	created := ts(0)
	completed := ts(100)
	sess := store.Session{
		ID: "sess-1", Goal: "do the thing", Worker: "claude",
		Status: "completed", CreatedAt: created, CompletedAt: &completed,
		ModeName: "dev",
	}
	st1Start, st1End := ts(10), ts(40)
	st2Start, st2End := ts(12), ts(55)
	subtasks := []store.Subtask{
		{
			ID: "st-1", SessionID: "sess-1", Title: "first part", Worker: "claude",
			Status: "completed", StartedAt: &st1Start, CompletedAt: &st1End,
			MetaJSON: `{"tokens_in":120,"tokens_out":80,"usd_cents":3}`,
		},
		{
			ID: "st-2", SessionID: "sess-1", Title: "second part", Worker: "gemini",
			Status: "failed", StartedAt: &st2Start, CompletedAt: &st2End,
			Error: "provider timeout", ErrorKind: "transport",
		},
		{
			ID: "st-3", SessionID: "sess-1", Title: "never ran", Worker: "claude",
			Status: "skipped", // no StartedAt — must not produce a span
		},
	}
	events := []store.EventRow{
		{SessionID: "sess-1", Seq: 1, Ts: ts(0), Kind: "goal_received"},
		{SessionID: "sess-1", Seq: 2, Ts: ts(2), Kind: "plan_requested"},
		{SessionID: "sess-1", Seq: 3, Ts: ts(8), Kind: "plan_proposed"},
		{SessionID: "sess-1", SubtaskID: "st-1", Seq: 4, Ts: ts(15), Kind: "subtask_tool_call"},
		{SessionID: "sess-1", SubtaskID: "st-1", Seq: 5, Ts: ts(16), Kind: "subtask_tool_result"},
		{SessionID: "sess-1", Seq: 6, Ts: ts(60), Kind: "synthesis_started"},
		{SessionID: "sess-1", Seq: 7, Ts: ts(90), Kind: "synthesis_completed"},
		{SessionID: "sess-1", Seq: 8, Ts: ts(100), Kind: "run_completed"},
	}
	return sess, subtasks, events
}

func allSpans(req ExportTraceServiceRequest) []Span {
	var out []Span
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			out = append(out, ss.Spans...)
		}
	}
	return out
}

func findSpan(t *testing.T, spans []Span, name string) Span {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("span %q not found in %d spans", name, len(spans))
	return Span{}
}

func attrString(s Span, key string) (string, bool) {
	for _, kv := range s.Attributes {
		if kv.Key == key && kv.Value.StringValue != nil {
			return *kv.Value.StringValue, true
		}
	}
	return "", false
}

func attrInt(s Span, key string) (string, bool) {
	for _, kv := range s.Attributes {
		if kv.Key == key && kv.Value.IntValue != nil {
			return *kv.Value.IntValue, true
		}
	}
	return "", false
}

func TestBuildTrace_SpanHierarchy(t *testing.T) {
	sess, subtasks, events := sampleSession()
	req := BuildTrace(sess, subtasks, events)
	spans := allSpans(req)

	// Root + plan + synthesis + 2 dispatched subtasks (st-3 skipped).
	if len(spans) != 5 {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name
		}
		t.Fatalf("got %d spans %v, want 5", len(spans), names)
	}

	root := findSpan(t, spans, "invoke_agent uta")
	if root.ParentSpanID != "" {
		t.Errorf("root has parent %q", root.ParentSpanID)
	}
	if root.Status.Code != StatusOK {
		t.Errorf("root status = %d, want OK", root.Status.Code)
	}
	if v, _ := attrString(root, "gen_ai.operation.name"); v != "invoke_agent" {
		t.Errorf("root gen_ai.operation.name = %q", v)
	}
	if v, _ := attrString(root, "uta.mode"); v != "dev" {
		t.Errorf("root uta.mode = %q", v)
	}

	for _, name := range []string{"plan", "synthesis", "subtask first part", "subtask second part"} {
		s := findSpan(t, spans, name)
		if s.ParentSpanID != root.SpanID {
			t.Errorf("%s parent = %q, want root %q", name, s.ParentSpanID, root.SpanID)
		}
		if s.TraceID != root.TraceID {
			t.Errorf("%s traceId = %q, want %q", name, s.TraceID, root.TraceID)
		}
	}
}

func TestBuildTrace_SubtaskAttributesAndStatus(t *testing.T) {
	sess, subtasks, events := sampleSession()
	spans := allSpans(BuildTrace(sess, subtasks, events))

	ok := findSpan(t, spans, "subtask first part")
	if v, _ := attrString(ok, "gen_ai.agent.name"); v != "claude" {
		t.Errorf("agent.name = %q", v)
	}
	if v, found := attrInt(ok, "gen_ai.usage.input_tokens"); !found || v != "120" {
		t.Errorf("input_tokens = %q found=%v", v, found)
	}
	if v, found := attrInt(ok, "gen_ai.usage.output_tokens"); !found || v != "80" {
		t.Errorf("output_tokens = %q found=%v", v, found)
	}
	if len(ok.Events) != 2 {
		t.Errorf("expected 2 tool span-events, got %d", len(ok.Events))
	}

	failed := findSpan(t, spans, "subtask second part")
	if failed.Status.Code != StatusError {
		t.Errorf("failed subtask status = %d, want ERROR", failed.Status.Code)
	}
	if failed.Status.Message != "provider timeout" {
		t.Errorf("status message = %q", failed.Status.Message)
	}
	if _, found := attrInt(failed, "gen_ai.usage.input_tokens"); found {
		t.Error("subtask without usage meta must not carry token attributes")
	}
}

func TestBuildTrace_DeterministicIDs(t *testing.T) {
	sess, subtasks, events := sampleSession()
	a := BuildTrace(sess, subtasks, events)
	b := BuildTrace(sess, subtasks, events)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Error("two exports of the same session differ — ids must be deterministic")
	}
	if TraceIDFromSession("x") == TraceIDFromSession("y") {
		t.Error("different sessions produced the same trace id")
	}
	if len(TraceIDFromSession("x")) != 32 || len(SpanIDFrom("x")) != 16 {
		t.Errorf("id lengths: trace=%d span=%d, want 32/16 hex chars",
			len(TraceIDFromSession("x")), len(SpanIDFrom("x")))
	}
}

// TestBuildTrace_OTLPJSONShape pins the wire-format details collectors
// are strict about: uint64 timestamps as strings, attribute values
// wrapped in {stringValue}/{intValue}, resource carrying service.name.
func TestBuildTrace_OTLPJSONShape(t *testing.T) {
	sess, subtasks, events := sampleSession()
	raw, err := json.Marshal(BuildTrace(sess, subtasks, events))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		`"resourceSpans"`, `"scopeSpans"`, `"traceId"`, `"spanId"`,
		`"startTimeUnixNano":"`, // string-encoded nanos
		`"stringValue":"uta"`,   // service.name attr
		`"intValue":"120"`,      // int64 as string
	} {
		if !strings.Contains(s, want) {
			t.Errorf("OTLP JSON missing %s", want)
		}
	}
}

func TestBuildTrace_EmptyTrajectory(t *testing.T) {
	created := ts(0)
	sess := store.Session{ID: "s", Goal: "g", Worker: "w", Status: "running", CreatedAt: created}
	spans := allSpans(BuildTrace(sess, nil, nil))
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1 (root only)", len(spans))
	}
	if spans[0].Status.Code != StatusUnset {
		t.Errorf("running session status = %d, want UNSET", spans[0].Status.Code)
	}
}
