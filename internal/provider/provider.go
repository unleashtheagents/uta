// Package provider defines the AgentProvider interface — the seam between uta's
// orchestrator and the agent CLIs it drives (Claude Code, Gemini CLI, etc).
//
// A provider is a thin adapter over a CLI: it knows how to detect the binary on
// PATH, how to run a single headless turn, how to parse that CLI's streamed
// output into uta's normalized Event stream, and (optionally) how to resume a
// prior session. New providers can be added by writing one Go file that
// implements AgentProvider, or by dropping a YAML descriptor into
// ~/.uta/providers/ that the declarative provider can interpret.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Capability is a free-form tag a provider advertises. The supervisor and CLI
// inspect these to decide what's safe to ask of a given provider.
type Capability string

const (
	CapResume         Capability = "resume"
	CapStreamJSON     Capability = "stream-json"
	CapToolPreApprove Capability = "tool-preapprove"
	CapMCP            Capability = "mcp"
)

// Detection is what Detect() returns. Available=false means the binary isn't
// on PATH or isn't usable; Notes carries human-readable hints (auth missing,
// version too old, etc).
type Detection struct {
	Available    bool
	BinaryPath   string
	Version      string
	Capabilities []Capability
	Notes        string
}

// RunOptions controls a single headless invocation.
type RunOptions struct {
	Workdir         string
	Env             []string      // appended to os.Environ()
	Timeout         time.Duration // 0 = inherit ctx
	PreApproveTools []string      // best-effort; ignored if provider lacks CapToolPreApprove
	ExtraArgs       []string      // escape hatch
}

// RunResult is the terminal value of one RunHeadless call. RawOutput holds
// the entire raw stdout the provider emitted (line-delimited JSON for stream
// modes); the supervisor is responsible for persisting it to blob storage and
// recording the resulting path.
type RunResult struct {
	SessionID string
	FinalText string
	RawOutput []byte
	ExitCode  int
}

// Event is one normalized event drained from a running provider. Providers
// stream these on the channel passed to RunHeadless; the supervisor publishes
// them onto the trajectory bus.
//
// Payload is provider-specific JSON. We do not normalize the inner shape here —
// the trajectory recorder stores it raw so we can rev the schema later without
// losing fidelity.
type Event struct {
	Kind      EventKind
	Timestamp time.Time
	Payload   json.RawMessage
}

// EventKind enumerates the normalized event types every provider must emit.
type EventKind string

const (
	EventSessionID     EventKind = "session_id"
	EventStdoutChunk   EventKind = "stdout_chunk"
	EventAssistantText EventKind = "assistant_text"
	EventToolCall      EventKind = "tool_call"
	EventToolResult    EventKind = "tool_result"
	EventError         EventKind = "error"
)

// AgentProvider is the mandatory contract for every backend.
type AgentProvider interface {
	Name() string
	Detect(ctx context.Context) Detection
	RunHeadless(ctx context.Context, prompt string, opts RunOptions, events chan<- Event) (RunResult, error)
}

// Resumable is an optional capability interface. Providers that advertise
// CapResume must implement it.
type Resumable interface {
	ResumeHeadless(ctx context.Context, sessionID, prompt string, opts RunOptions, events chan<- Event) (RunResult, error)
}

// Error sentinels. Wrap with fmt.Errorf("...: %w", ErrXxx) so errors.Is works.
var (
	ErrAuth         = errors.New("provider auth failure")
	ErrTransport    = errors.New("provider transport failure")
	ErrWorkerFailed = errors.New("worker reported failure")
	ErrTimeout      = errors.New("provider timed out")
	ErrUnsupported  = errors.New("capability unsupported")
	ErrQuota        = errors.New("provider quota exceeded")
)
