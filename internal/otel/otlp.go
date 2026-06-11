// Package otel maps uta trajectories onto OpenTelemetry traces using
// the OTLP/JSON wire shape and the GenAI semantic conventions (agent /
// tool spans + token-usage attributes). Hand-rolled structs instead of
// the OTel SDK: uta only ever EMITS one fixed shape, the SDK would be
// the largest dependency in the module, and the OTLP/JSON encoding is
// stable and documented.
//
// The output of BuildTrace marshals to a valid ExportTraceServiceRequest
// — POST it to any collector's /v1/traces (Jaeger, Langfuse, Phoenix,
// Laminar, otel-collector) or store it as a file.
package otel

import (
	"crypto/sha256"
	"encoding/hex"
)

// ExportTraceServiceRequest is the root OTLP/JSON document.
type ExportTraceServiceRequest struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

type ScopeSpans struct {
	Scope Scope  `json:"scope"`
	Spans []Span `json:"spans"`
}

type Scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Span is one OTLP span. Times are unix nanos serialized as strings
// (OTLP/JSON encodes uint64 as JSON string).
type Span struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	ParentSpanID      string      `json:"parentSpanId,omitempty"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"` // 1 = INTERNAL
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []KeyValue  `json:"attributes,omitempty"`
	Events            []SpanEvent `json:"events,omitempty"`
	Status            SpanStatus  `json:"status"`
}

// SpanEvent is a point-in-time annotation on a span (tool calls, gate
// outcomes, budget warnings).
type SpanEvent struct {
	TimeUnixNano string     `json:"timeUnixNano"`
	Name         string     `json:"name"`
	Attributes   []KeyValue `json:"attributes,omitempty"`
}

// SpanStatus per OTLP: code 0 = UNSET, 1 = OK, 2 = ERROR.
type SpanStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

const (
	StatusUnset = 0
	StatusOK    = 1
	StatusError = 2

	SpanKindInternal = 1
)

// KeyValue is the OTLP attribute pair.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue holds exactly one of the value fields.
type AnyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"` // OTLP/JSON: int64 as string
	BoolValue   *bool   `json:"boolValue,omitempty"`
}

// Str builds a string attribute.
func Str(key, val string) KeyValue {
	return KeyValue{Key: key, Value: AnyValue{StringValue: &val}}
}

// Int builds an int attribute (OTLP/JSON wants int64 as a string).
func Int(key string, val int64) KeyValue {
	s := itoa64(val)
	return KeyValue{Key: key, Value: AnyValue{IntValue: &s}}
}

// TraceIDFromSession derives a deterministic 16-byte (32 hex char)
// trace id from the uta session id. Deterministic on purpose: exporting
// the same session twice must produce the same trace so collectors
// dedup instead of duplicating.
func TraceIDFromSession(sessionID string) string {
	sum := sha256.Sum256([]byte("uta-trace:" + sessionID))
	return hex.EncodeToString(sum[:16])
}

// SpanIDFrom derives a deterministic 8-byte (16 hex char) span id from
// any stable identifier (subtask id, "planner:<session>", ...).
func SpanIDFrom(id string) string {
	sum := sha256.Sum256([]byte("uta-span:" + id))
	return hex.EncodeToString(sum[:8])
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
