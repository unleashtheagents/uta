// Package sentinel is the trajectory watcher. It subscribes to a
// trajectory.Bus, applies cheap heuristic rules, and emits sentinel_alert
// events when something looks wrong (loops, error storms, runaway cost,
// suspicious write paths). When a rule's severity is "critical", the
// Sentinel calls the registered cancel hook so the supervisor can halt the
// run cleanly.
//
// The Sentinel is *not* an LLM. It's deterministic, cheap, and runs in the
// same process. A future Sentinel-as-LLM (Haiku-class watcher) can be added
// later as an additional subscriber.
package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

// Alert is what the Sentinel publishes back onto the bus when a rule
// matches. Severity ranks: info < warn < critical. Critical triggers the
// cancel hook (if any).
type Alert struct {
	Rule     string         `json:"rule"`
	Severity string         `json:"severity"` // info|warn|critical
	Message  string         `json:"message"`
	Context  map[string]any `json:"context,omitempty"`
	Ts       time.Time      `json:"ts"`
}

// Recorder is the minimal slice of trajectory.Recorder that Sentinel uses to
// persist its own alerts as trajectory_events. Sentinel takes this rather
// than the full Recorder type to keep its dep graph narrow.
type Recorder interface {
	AllocSeq(sessionID string) int64
	PersistSync(ev trajectory.Event)
}

// Config controls the Sentinel's behavior.
type Config struct {
	// BudgetWallClock, when non-zero, raises an alert at 80% elapsed and
	// a critical alert at 100% (which triggers Cancel).
	BudgetWallClock time.Duration

	// RepeatedToolThreshold is how many consecutive identical tool_call
	// payloads count as a loop. Default 8.
	RepeatedToolThreshold int

	// ErrorRateWindow is the sliding window for error-rate detection.
	// Within ErrorRateWindow events, if more than ErrorRateMax events have
	// kind ending in _failed, raise warn.
	ErrorRateWindow int
	ErrorRateMax    int

	// SensitivePathSubstrings: when a tool_call payload contains one of
	// these as a string anywhere in the JSON, raise warn. Defaults to a
	// reasonable list if nil.
	SensitivePathSubstrings []string

	// Cancel is the hook invoked when a critical alert fires. If nil, the
	// alert is published but the run is allowed to continue.
	Cancel context.CancelFunc

	// PublishOnBus, when non-nil, fans alerts out to any other live
	// subscribers (e.g. the progress renderer).
	PublishOnBus *trajectory.Bus

	// Recorder, when non-nil, persists alerts as trajectory_events so they
	// show up in `uta trajectory <id>` and survive past the in-process bus.
	Recorder Recorder
}

// Sentinel is one watcher attached to a Bus.
type Sentinel struct {
	cfg        Config
	startedAt  time.Time
	alerted80  atomic.Bool
	alertedMax atomic.Bool

	mu             sync.Mutex
	recentTools    []string
	recentEventLog []string // kinds, capped at ErrorRateWindow
	sessionID      string   // bound at Watch() start

	subs   []chan Alert
	subsMu sync.Mutex
}

// NewSentinel returns a Sentinel with config defaults filled in.
func NewSentinel(cfg Config) *Sentinel {
	if cfg.RepeatedToolThreshold <= 0 {
		cfg.RepeatedToolThreshold = 8
	}
	if cfg.ErrorRateWindow <= 0 {
		cfg.ErrorRateWindow = 20
	}
	if cfg.ErrorRateMax <= 0 {
		cfg.ErrorRateMax = 5
	}
	if cfg.SensitivePathSubstrings == nil {
		cfg.SensitivePathSubstrings = DefaultSensitivePaths
	}
	return &Sentinel{cfg: cfg, startedAt: time.Now()}
}

// DefaultSensitivePaths is the conservative starter list.
var DefaultSensitivePaths = []string{
	"/etc/",
	"/.ssh/",
	"/.aws/",
	"/.kube/",
	"/.envrc",
	"/.env",
	"id_rsa",
	"id_ed25519",
	"/private/",
	"/keychain",
}

// Subscribe returns a channel that receives every alert. Buffered.
func (s *Sentinel) Subscribe(buffer int) <-chan Alert {
	if buffer <= 0 {
		buffer = 32
	}
	ch := make(chan Alert, buffer)
	s.subsMu.Lock()
	s.subs = append(s.subs, ch)
	s.subsMu.Unlock()
	return ch
}

// Watch starts the watcher on the supplied bus. Blocks until ctx is done or
// the bus closes its subscription channel. Designed to be run in a goroutine:
//
//	go sentinel.Watch(ctx, bus)
func (s *Sentinel) Watch(ctx context.Context, bus *trajectory.Bus) {
	if bus == nil {
		return
	}
	ch := bus.Subscribe(256)
	// 1s tick is responsive enough for short budgets (audit --budget-time 30s)
	// without being noisy on long runs.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.process(ev)
		case <-ticker.C:
			s.tickBudget()
		}
	}
}

func (s *Sentinel) process(ev trajectory.Event) {
	if s.sessionID == "" {
		s.sessionID = ev.SessionID
	}

	// 1. Repeated tool-call loop detection.
	if ev.Kind == trajectory.SubtaskToolCall {
		key := engine.TruncateBytes(ev.Payload, 256)
		s.mu.Lock()
		s.recentTools = append(s.recentTools, key)
		if len(s.recentTools) > s.cfg.RepeatedToolThreshold*2 {
			s.recentTools = s.recentTools[len(s.recentTools)-s.cfg.RepeatedToolThreshold*2:]
		}
		// count consecutive trailing equals
		streak := 1
		for i := len(s.recentTools) - 2; i >= 0; i-- {
			if s.recentTools[i] == key {
				streak++
			} else {
				break
			}
		}
		s.mu.Unlock()
		if streak >= s.cfg.RepeatedToolThreshold {
			s.fire(Alert{
				Rule:     "repeated_tool_loop",
				Severity: "warn",
				Message:  fmt.Sprintf("same tool call %d times in a row — possible loop", streak),
				Context:  map[string]any{"streak": streak, "session": s.sessionID},
			})
		}
	}

	// 2. Error-rate spike.
	if isErrorEvent(ev.Kind) {
		s.mu.Lock()
		s.recentEventLog = append(s.recentEventLog, string(ev.Kind))
		if len(s.recentEventLog) > s.cfg.ErrorRateWindow {
			s.recentEventLog = s.recentEventLog[len(s.recentEventLog)-s.cfg.ErrorRateWindow:]
		}
		errCount := 0
		for _, k := range s.recentEventLog {
			if isErrorEventKind(k) {
				errCount++
			}
		}
		s.mu.Unlock()
		if errCount >= s.cfg.ErrorRateMax {
			s.fire(Alert{
				Rule:     "error_rate_spike",
				Severity: "warn",
				Message:  fmt.Sprintf("%d failed events in last %d", errCount, s.cfg.ErrorRateWindow),
				Context:  map[string]any{"count": errCount, "window": s.cfg.ErrorRateWindow},
			})
		}
	}

	// 3. Sensitive-path heuristic on tool_call/tool_result payloads.
	if ev.Kind == trajectory.SubtaskToolCall || ev.Kind == trajectory.SubtaskToolResult {
		payload := string(ev.Payload)
		low := strings.ToLower(payload)
		for _, sub := range s.cfg.SensitivePathSubstrings {
			if strings.Contains(low, strings.ToLower(sub)) {
				s.fire(Alert{
					Rule:     "sensitive_path",
					Severity: "warn",
					Message:  fmt.Sprintf("subtask referenced sensitive path: %s", sub),
					Context:  map[string]any{"match": sub, "kind": string(ev.Kind)},
				})
				break
			}
		}
	}
}

func (s *Sentinel) tickBudget() {
	if s.cfg.BudgetWallClock <= 0 {
		return
	}
	elapsed := time.Since(s.startedAt)
	frac := float64(elapsed) / float64(s.cfg.BudgetWallClock)
	if frac >= 1.0 && !s.alertedMax.Load() {
		s.alertedMax.Store(true)
		s.fire(Alert{
			Rule:     "budget_exhausted",
			Severity: "critical",
			Message:  fmt.Sprintf("wall-clock budget %s exhausted (elapsed %s) — cancelling run", s.cfg.BudgetWallClock, elapsed.Round(time.Second)),
			Context:  map[string]any{"elapsed_ms": elapsed.Milliseconds(), "budget_ms": s.cfg.BudgetWallClock.Milliseconds()},
		})
		if s.cfg.Cancel != nil {
			s.cfg.Cancel()
		}
		return
	}
	if frac >= 0.8 && !s.alerted80.Load() {
		s.alerted80.Store(true)
		s.fire(Alert{
			Rule:     "budget_warning",
			Severity: "warn",
			Message:  fmt.Sprintf("wall-clock budget 80%% consumed (%s of %s)", elapsed.Round(time.Second), s.cfg.BudgetWallClock),
			Context:  map[string]any{"elapsed_ms": elapsed.Milliseconds(), "budget_ms": s.cfg.BudgetWallClock.Milliseconds()},
		})
	}
}

func (s *Sentinel) fire(a Alert) {
	a.Ts = time.Now()
	// Publish to all subscribers (non-blocking; drop on full buffer).
	s.subsMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- a:
		default:
		}
	}
	s.subsMu.Unlock()
	// Persist + republish onto the trajectory bus as an event so it shows
	// up in `uta trajectory <id>` and the renderer.
	if s.sessionID == "" {
		return
	}
	payload, _ := json.Marshal(a)
	var seq int64
	if s.cfg.Recorder != nil {
		seq = s.cfg.Recorder.AllocSeq(s.sessionID)
	}
	ev := trajectory.Event{
		SessionID: s.sessionID,
		Seq:       seq,
		Ts:        a.Ts,
		Kind:      trajectory.SentinelAlert,
		Payload:   payload,
	}
	if s.cfg.Recorder != nil {
		s.cfg.Recorder.PersistSync(ev)
	}
	if s.cfg.PublishOnBus != nil {
		s.cfg.PublishOnBus.Publish(ev)
	}
}

func isErrorEvent(k trajectory.Kind) bool {
	switch k {
	case trajectory.SubtaskFailed,
		trajectory.GateFailed,
		trajectory.ToolFailed,
		trajectory.PlanFallback,
		trajectory.RunFailed:
		return true
	}
	return false
}

func isErrorEventKind(k string) bool {
	return strings.HasSuffix(k, "_failed") || k == "plan_fallback" || k == "run_failed"
}

