// Package hitl is the human-in-the-loop signature gate. For high-stakes
// actions, uta pauses and asks a human for [y/N] before letting the
// orchestration proceed.
//
// Two flows:
//
//   - Synchronous (TTY): when stdin is a terminal, the gate prints the
//     prompt + detail to stderr and reads y/N from stdin. v1 single-approver.
//
//   - Asynchronous (file-drop): when stdin is /dev/null or otherwise not a
//     TTY, the gate writes a `pending.json` under
//     <state>/.uta/hitl/<session>/<action>.pending.json and blocks until
//     a sibling `<action>.approved` or `<action>.denied` file appears.
//     The `uta hitl approve <session> [--action <id>]` command writes that
//     marker.
//
// The gate is library-shaped — no flag parsing, no global state — so the
// supervisor can call it from event-drain goroutines without coupling to
// cobra or os.Args. Tests inject a deterministic Approver via the
// Approver interface.
//
// Out of scope for v1: multi-signature (two humans approve) and per-trigger
// approver routing. Single-approver only.
package hitl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/mattn/go-isatty"
)

// Request is a single human-approval ask.
type Request struct {
	SessionID  string      `json:"session_id"`
	SubtaskID  string      `json:"subtask_id,omitempty"`
	Action     string      `json:"action"`           // logical name: "tool_call" | "budget_threshold" | ...
	Severity   string      `json:"severity"`         // info|low|medium|high|critical
	PromptText string      `json:"prompt"`           // shown to the human verbatim
	Detail     interface{} `json:"detail,omitempty"` // marshaled into JSON for the pending file
}

// Pending is the on-disk shape of a HITL pending request — the formal
// schema for `<session-dir>/<action-id>.pending.json` and the
// `<session-dir>/pending.json` "newest pending" pointer. External tools
// (the `uta hitl approve` command, dashboards) read and parse this struct.
// Adding fields here without bumping the consumer is safe; removing or
// renaming fields is not.
type Pending struct {
	ActionID    string          `json:"action_id"`
	SessionID   string          `json:"session_id"`
	SubtaskID   string          `json:"subtask_id,omitempty"`
	Action      string          `json:"action"`
	Severity    string          `json:"severity,omitempty"`
	Prompt      string          `json:"prompt,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	RequestedAt string          `json:"requested_at"`
}

// Decision is the outcome the supervisor consumes.
type Decision struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
	Approver string `json:"approver"` // "tty" | "async" | "test"
	ActionID string `json:"action_id"`
}

// Approver asks a human and reports back. Implementations:
//   - *Gate: production TTY + file-drop hybrid
//   - StaticApprover: deterministic test double
type Approver interface {
	Request(ctx context.Context, r Request) (Decision, error)
}

// Gate is the production HITLGate. It auto-detects TTY (synchronous prompt)
// and falls back to the file-drop flow when stdin is not a terminal.
type Gate struct {
	// StateDir is the directory under which pending/approved/denied files
	// are written. Final path: <StateDir>/hitl/<session>/<action>.*.
	// Required for the async flow.
	StateDir string

	// Stdin / Stderr override the defaults (os.Stdin / os.Stderr). Tests
	// inject buffers; the CLI leaves them nil.
	Stdin  io.Reader
	Stderr io.Writer

	// ForceAsync skips TTY detection. Used to verify the file-drop flow
	// from tests even when the test process happens to inherit a TTY.
	ForceAsync bool

	// AsyncPoll is the polling interval for the file-drop loop. 0 picks a
	// sensible default (500ms).
	AsyncPoll time.Duration
}

// Request implements Approver. Returns the decision (or a ctx.Err()
// wrapping context.Canceled when the supervisor cancels mid-prompt).
func (g *Gate) Request(ctx context.Context, r Request) (Decision, error) {
	if g == nil {
		return Decision{}, errors.New("nil gate")
	}
	actionID := uuid.NewString()
	r.Action = strings.TrimSpace(r.Action)
	if r.Action == "" {
		r.Action = "hitl"
	}
	stderr := g.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	stdin := g.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}

	if !g.ForceAsync && isTerminal(stdin) {
		return g.runTTY(ctx, r, stdin, stderr, actionID)
	}
	return g.runAsync(ctx, r, stderr, actionID)
}

// runTTY prompts to stderr and reads one line from stdin. A bare "y"
// or "yes" (case-insensitive) approves; anything else (including empty
// input or EOF) denies. Returns ctx.Err() if cancellation arrives while
// we're waiting on the reader.
func (g *Gate) runTTY(ctx context.Context, r Request, stdin io.Reader, stderr io.Writer, actionID string) (Decision, error) {
	fmt.Fprintf(stderr, "\n[uta:hitl] action=%s severity=%s\n", actionID, dashIfEmpty(r.Severity))
	if r.PromptText != "" {
		fmt.Fprintln(stderr, r.PromptText)
	}
	if r.Detail != nil {
		if b, err := json.MarshalIndent(r.Detail, "", "  "); err == nil {
			fmt.Fprintln(stderr, "--- detail ---")
			fmt.Fprintln(stderr, string(b))
		}
	}
	fmt.Fprint(stderr, "Approve? [y/N]: ")

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		br := bufio.NewReader(stdin)
		line, err := br.ReadString('\n')
		ch <- readResult{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return Decision{Approver: "tty", ActionID: actionID, Reason: "cancelled"}, ctx.Err()
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			return Decision{Approver: "tty", ActionID: actionID, Reason: "stdin error: " + res.err.Error()}, nil
		}
		ans := strings.ToLower(strings.TrimSpace(res.line))
		if ans == "y" || ans == "yes" {
			return Decision{Approved: true, Approver: "tty", ActionID: actionID}, nil
		}
		return Decision{Approver: "tty", ActionID: actionID, Reason: "denied at TTY prompt"}, nil
	}
}

// runAsync writes pending.json + <action>.pending.json and polls for the
// matching .approved / .denied marker.
func (g *Gate) runAsync(ctx context.Context, r Request, stderr io.Writer, actionID string) (Decision, error) {
	if strings.TrimSpace(g.StateDir) == "" {
		return Decision{Approver: "async", ActionID: actionID, Reason: "async approver misconfigured"},
			errors.New("hitl: StateDir is required for the async flow")
	}
	if strings.TrimSpace(r.SessionID) == "" {
		return Decision{Approver: "async", ActionID: actionID, Reason: "async approver misconfigured"},
			errors.New("hitl: SessionID is required for the async flow")
	}
	dir := SessionDir(g.StateDir, r.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Decision{Approver: "async", ActionID: actionID}, fmt.Errorf("hitl dir: %w", err)
	}
	pending := PendingPath(dir, actionID)
	var detail json.RawMessage
	if r.Detail != nil {
		raw, mErr := json.Marshal(r.Detail)
		if mErr != nil {
			return Decision{Approver: "async", ActionID: actionID}, fmt.Errorf("marshal detail: %w", mErr)
		}
		detail = raw
	}
	body, err := json.MarshalIndent(Pending{
		ActionID:    actionID,
		SessionID:   r.SessionID,
		SubtaskID:   r.SubtaskID,
		Action:      r.Action,
		Severity:    r.Severity,
		Prompt:      r.PromptText,
		Detail:      detail,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return Decision{Approver: "async", ActionID: actionID}, fmt.Errorf("marshal pending: %w", err)
	}
	if err := os.WriteFile(pending, body, 0o644); err != nil {
		return Decision{Approver: "async", ActionID: actionID}, fmt.Errorf("write pending: %w", err)
	}
	// Mirror as a stable pointer for the "first pending" lookup used by
	// `uta hitl approve <session>` without --action. The per-action file
	// is the authoritative record, so a pointer write failure is logged
	// but does not abort the request — the operator can still approve
	// with --action <id>.
	if werr := os.WriteFile(filepath.Join(dir, "pending.json"), body, 0o644); werr != nil && stderr != nil {
		fmt.Fprintf(stderr, "[uta:hitl] warning: failed to write pending.json pointer in %s: %v (use --action %s to approve)\n", dir, werr, actionID)
	}
	defer func() {
		_ = os.Remove(pending)
		// Only clear the stable pointer when it still names *this* action.
		if b, err := os.ReadFile(filepath.Join(dir, "pending.json")); err == nil {
			var pp Pending
			if jerr := json.Unmarshal(b, &pp); jerr == nil && pp.ActionID == actionID {
				_ = os.Remove(filepath.Join(dir, "pending.json"))
			}
		}
	}()

	if stderr != nil {
		fmt.Fprintf(stderr, "[uta:hitl] awaiting approval — run: uta hitl approve %s --action %s\n", r.SessionID, actionID)
	}

	approved := ApprovedPath(dir, actionID)
	denied := DeniedPath(dir, actionID)

	poll := g.AsyncPoll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Decision{Approver: "async", ActionID: actionID, Reason: "cancelled"}, ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(approved); err == nil {
				_ = os.Remove(approved)
				return Decision{Approved: true, Approver: "async", ActionID: actionID}, nil
			}
			if _, err := os.Stat(denied); err == nil {
				reason := "denied via file-drop"
				if b, rerr := os.ReadFile(denied); rerr == nil {
					if s := strings.TrimSpace(string(b)); s != "" {
						reason = s
					}
				}
				_ = os.Remove(denied)
				return Decision{Approver: "async", ActionID: actionID, Reason: reason}, nil
			}
		}
	}
}

// StaticApprover is a deterministic Approver used in tests and by
// non-interactive callers that need to bypass HITL entirely (e.g.
// `--yes`). The Calls counter is bumped on every invocation so tests can
// assert how often the gate was consulted.
type StaticApprover struct {
	Approve bool
	Reason  string
	calls   atomic.Int64
}

// Request implements Approver. ActionID is generated per call so trajectory
// events still have a unique handle.
func (s *StaticApprover) Request(_ context.Context, r Request) (Decision, error) {
	s.calls.Add(1)
	if s.Approve {
		return Decision{Approved: true, Approver: "test", ActionID: uuid.NewString()}, nil
	}
	reason := s.Reason
	if reason == "" {
		reason = "denied by StaticApprover"
	}
	return Decision{Approver: "test", ActionID: uuid.NewString(), Reason: reason}, nil
}

// Calls returns the number of times Request has been invoked.
func (s *StaticApprover) Calls() int64 { return s.calls.Load() }

// SessionDir returns the on-disk directory uta uses for HITL artefacts of
// the given session. Exposed so the CLI's `uta hitl approve` command can
// resolve the same path the gate writes to.
func SessionDir(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "hitl", sessionID)
}

// PendingPath returns the path of the per-action pending file.
func PendingPath(sessionDir, actionID string) string {
	return filepath.Join(sessionDir, actionID+".pending.json")
}

// ApprovedPath returns the path of the per-action approval marker.
func ApprovedPath(sessionDir, actionID string) string {
	return filepath.Join(sessionDir, actionID+".approved")
}

// DeniedPath returns the path of the per-action denial marker. The file's
// contents (if non-empty) are surfaced as the Decision.Reason.
func DeniedPath(sessionDir, actionID string) string {
	return filepath.Join(sessionDir, actionID+".denied")
}

// FirstPending reads <sessionDir>/pending.json and returns its action_id,
// for callers that want to approve "whatever is waiting." Empty string +
// nil error when there is no pending action.
func FirstPending(sessionDir string) (string, error) {
	p, err := ReadPending(sessionDir)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", nil
	}
	return p.ActionID, nil
}

// ReadPending parses <sessionDir>/pending.json into a *Pending. Returns
// (nil, nil) when no pending action is on disk. Use this from external
// tools (CLI, dashboard) that want to inspect what's waiting, not just
// approve blindly.
func ReadPending(sessionDir string) (*Pending, error) {
	b, err := os.ReadFile(filepath.Join(sessionDir, "pending.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse pending.json: %w", err)
	}
	return &p, nil
}

// ListPending walks <stateDir>/hitl/<session>/pending.json across every
// session directory and returns the parsed Pending records. Sessions with
// no pending pointer are skipped silently; unreadable / malformed
// pending.json files are skipped with no error so a single corrupt file
// can't block listing the rest. Returns (nil, nil) when <stateDir>/hitl
// does not exist.
func ListPending(stateDir string) ([]*Pending, error) {
	root := filepath.Join(stateDir, "hitl")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Pending
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := ReadPending(filepath.Join(root, e.Name()))
		if err != nil || p == nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// WriteApproval drops the .approved marker for an action. Used by
// `uta hitl approve` (or any external orchestrator) to unblock the gate's
// async polling loop.
func WriteApproval(sessionDir, actionID string) error {
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(ApprovedPath(sessionDir, actionID), []byte{}, 0o644)
}

// WriteDenial drops the .denied marker with an optional reason. Empty
// reason still denies — the file's mere existence is the signal.
func WriteDenial(sessionDir, actionID, reason string) error {
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(DeniedPath(sessionDir, actionID), []byte(reason), 0o644)
}

// isTerminal reports whether r is a *os.File pointing at a TTY.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd())
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
