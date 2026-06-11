package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/trajectory"
)

func waitAlert(t *testing.T, ch <-chan Alert) Alert {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for alert")
		return Alert{}
	}
}

func expectNoAlert(t *testing.T, ch <-chan Alert, d time.Duration) {
	t.Helper()
	select {
	case a := <-ch:
		t.Fatalf("unexpected alert: %+v", a)
	case <-time.After(d):
	}
}

func TestNewSentinel_FillsDefaults(t *testing.T) {
	s := NewSentinel(Config{})
	if s.cfg.RepeatedToolThreshold != 8 {
		t.Errorf("RepeatedToolThreshold default: got %d want 8", s.cfg.RepeatedToolThreshold)
	}
	if s.cfg.ErrorRateWindow != 20 {
		t.Errorf("ErrorRateWindow default: got %d want 20", s.cfg.ErrorRateWindow)
	}
	if s.cfg.ErrorRateMax != 5 {
		t.Errorf("ErrorRateMax default: got %d want 5", s.cfg.ErrorRateMax)
	}
	if len(s.cfg.SensitivePathSubstrings) == 0 {
		t.Error("SensitivePathSubstrings default: empty")
	}
}

func TestSentinel_BudgetCritical_FiresAndCancels(t *testing.T) {
	cancelled := false
	s := NewSentinel(Config{
		BudgetWallClock: 10 * time.Second,
		Cancel:          func() { cancelled = true },
	})
	sub := s.Subscribe(8)
	// Pretend we started long ago so we're well past the budget.
	s.startedAt = time.Now().Add(-time.Hour)
	s.tickBudget()

	a := waitAlert(t, sub)
	if a.Rule != "budget_exhausted" {
		t.Errorf("rule: got %q want budget_exhausted", a.Rule)
	}
	if a.Severity != "critical" {
		t.Errorf("severity: got %q want critical", a.Severity)
	}
	if !cancelled {
		t.Error("cancel hook not invoked on critical budget")
	}

	// alertedMax latches — a second tick must not re-fire the critical alert.
	// (A warn may still appear since alerted80 wasn't set when we jumped
	// straight to critical — we only care that the critical doesn't repeat.)
	s.tickBudget()
	select {
	case a := <-sub:
		if a.Rule == "budget_exhausted" {
			t.Fatalf("critical re-fired: %+v", a)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSentinel_BudgetCritical_NoCancelHookIsTolerated(t *testing.T) {
	s := NewSentinel(Config{BudgetWallClock: 1 * time.Second})
	sub := s.Subscribe(8)
	s.startedAt = time.Now().Add(-time.Hour)
	// Must not panic when Cancel is nil.
	s.tickBudget()
	a := waitAlert(t, sub)
	if a.Severity != "critical" {
		t.Errorf("severity: got %q want critical", a.Severity)
	}
}

func TestSentinel_BudgetWarn_FiresOnceAt80pct(t *testing.T) {
	s := NewSentinel(Config{BudgetWallClock: 10 * time.Second})
	sub := s.Subscribe(8)
	s.startedAt = time.Now().Add(-9 * time.Second) // 90% elapsed
	s.tickBudget()

	a := waitAlert(t, sub)
	if a.Rule != "budget_warning" {
		t.Errorf("rule: got %q want budget_warning", a.Rule)
	}
	if a.Severity != "warn" {
		t.Errorf("severity: got %q want warn", a.Severity)
	}

	// Second tick at same state should not re-fire.
	s.tickBudget()
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_BudgetWarn_NoFireBelow80pct(t *testing.T) {
	s := NewSentinel(Config{BudgetWallClock: 10 * time.Second})
	sub := s.Subscribe(8)
	s.startedAt = time.Now().Add(-5 * time.Second) // 50% elapsed
	s.tickBudget()
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_BudgetDisabled_NeverFires(t *testing.T) {
	s := NewSentinel(Config{}) // BudgetWallClock == 0
	sub := s.Subscribe(8)
	s.startedAt = time.Now().Add(-time.Hour)
	s.tickBudget()
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_RepeatedToolLoop_FiresAtThreshold(t *testing.T) {
	s := NewSentinel(Config{RepeatedToolThreshold: 3})
	sub := s.Subscribe(8)
	payload := json.RawMessage(`{"tool":"shell","args":{"cmd":"ls"}}`)
	ev := trajectory.Event{SessionID: "sess-1", Kind: trajectory.SubtaskToolCall, Payload: payload}

	// Two calls — streak 2 < threshold 3, no alert.
	s.process(ev)
	s.process(ev)
	expectNoAlert(t, sub, 50*time.Millisecond)

	// Third match: streak 3 → fires.
	s.process(ev)
	a := waitAlert(t, sub)
	if a.Rule != "repeated_tool_loop" {
		t.Errorf("rule: got %q want repeated_tool_loop", a.Rule)
	}
	if a.Severity != "warn" {
		t.Errorf("severity: got %q want warn", a.Severity)
	}
	if got, _ := a.Context["streak"].(int); got != 3 {
		t.Errorf("streak context: got %v want 3", a.Context["streak"])
	}
}

func TestSentinel_RepeatedToolLoop_DifferentPayloadsDoNotTrip(t *testing.T) {
	s := NewSentinel(Config{RepeatedToolThreshold: 3})
	sub := s.Subscribe(8)
	for i := 0; i < 5; i++ {
		ev := trajectory.Event{
			SessionID: "sess-1",
			Kind:      trajectory.SubtaskToolCall,
			Payload:   json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		}
		s.process(ev)
	}
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_RepeatedToolLoop_StreakResetsOnDifferentPayload(t *testing.T) {
	s := NewSentinel(Config{RepeatedToolThreshold: 3})
	sub := s.Subscribe(8)
	same := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.SubtaskToolCall,
		Payload:   json.RawMessage(`{"a":1}`),
	}
	other := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.SubtaskToolCall,
		Payload:   json.RawMessage(`{"a":2}`),
	}
	// 2× same, 1× other, 2× same — streak is only 2 at the end, no alert.
	s.process(same)
	s.process(same)
	s.process(other)
	s.process(same)
	s.process(same)
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_ErrorRateSpike_FiresOnThirdFailure(t *testing.T) {
	s := NewSentinel(Config{ErrorRateWindow: 10, ErrorRateMax: 3})
	sub := s.Subscribe(8)
	ev := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.SubtaskFailed,
		Payload:   json.RawMessage(`{}`),
	}

	s.process(ev)
	s.process(ev)
	expectNoAlert(t, sub, 50*time.Millisecond)

	s.process(ev)
	a := waitAlert(t, sub)
	if a.Rule != "error_rate_spike" {
		t.Errorf("rule: got %q want error_rate_spike", a.Rule)
	}
	if a.Severity != "warn" {
		t.Errorf("severity: got %q want warn", a.Severity)
	}
}

func TestSentinel_ErrorRateSpike_CountsMixedErrorKinds(t *testing.T) {
	s := NewSentinel(Config{ErrorRateWindow: 10, ErrorRateMax: 3})
	sub := s.Subscribe(8)
	for _, k := range []trajectory.Kind{
		trajectory.SubtaskFailed,
		trajectory.GateFailed,
		trajectory.ToolFailed,
	} {
		s.process(trajectory.Event{SessionID: "sess-1", Kind: k, Payload: json.RawMessage(`{}`)})
	}
	a := waitAlert(t, sub)
	if a.Rule != "error_rate_spike" {
		t.Errorf("rule: got %q want error_rate_spike", a.Rule)
	}
}

func TestSentinel_ErrorRateSpike_NonErrorKindsIgnored(t *testing.T) {
	s := NewSentinel(Config{ErrorRateWindow: 10, ErrorRateMax: 3})
	sub := s.Subscribe(8)
	for i := 0; i < 5; i++ {
		s.process(trajectory.Event{
			SessionID: "sess-1",
			Kind:      trajectory.SubtaskCompleted,
			Payload:   json.RawMessage(`{}`),
		})
	}
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_CapabilityDeny_FiresAtThreshold(t *testing.T) {
	s := NewSentinel(Config{CapabilityDenyThreshold: 3, CapabilityDenyWindow: 10})
	sub := s.Subscribe(8)
	ev := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.CapabilityGateDenied,
		Payload:   json.RawMessage(`{"tool":"Bash","pattern":"Bash(curl *)"}`),
	}
	// First two denials don't trip the threshold.
	s.process(ev)
	s.process(ev)
	expectNoAlert(t, sub, 50*time.Millisecond)

	// Third pushes count to 3 and fires a warn alert.
	s.process(ev)
	a := waitAlert(t, sub)
	if a.Rule != "capability_gate_probing" {
		t.Errorf("rule: got %q want capability_gate_probing", a.Rule)
	}
	if a.Severity != "warn" {
		t.Errorf("severity: got %q want warn", a.Severity)
	}
	if got, _ := a.Context["denials"].(int); got != 3 {
		t.Errorf("denials context: got %v want 3", a.Context["denials"])
	}
}

func TestSentinel_CapabilityDeny_DoesNotFireOnUnrelatedEvents(t *testing.T) {
	s := NewSentinel(Config{CapabilityDenyThreshold: 3, CapabilityDenyWindow: 10})
	sub := s.Subscribe(8)
	for i := 0; i < 10; i++ {
		s.process(trajectory.Event{
			SessionID: "sess-1",
			Kind:      trajectory.SubtaskCompleted,
			Payload:   json.RawMessage(`{}`),
		})
	}
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_CapabilityDeny_WindowEvictsOldDenials(t *testing.T) {
	s := NewSentinel(Config{CapabilityDenyThreshold: 3, CapabilityDenyWindow: 5})
	sub := s.Subscribe(8)
	deny := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.CapabilityGateDenied,
		Payload:   json.RawMessage(`{}`),
	}
	noise := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.SubtaskCompleted,
		Payload:   json.RawMessage(`{}`),
	}
	// 2 denies then 5 unrelated events — denies fall out of the window.
	s.process(deny)
	s.process(deny)
	for i := 0; i < 5; i++ {
		s.process(noise)
	}
	// A single fresh deny should NOT trip the threshold (only 1 in window).
	s.process(deny)
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_SensitivePath_FiresOnSshKey(t *testing.T) {
	s := NewSentinel(Config{})
	sub := s.Subscribe(8)
	ev := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.SubtaskToolCall,
		Payload:   json.RawMessage(`{"path":"/Users/x/.ssh/id_rsa"}`),
	}
	s.process(ev)
	a := waitAlert(t, sub)
	if a.Rule != "sensitive_path" {
		t.Errorf("rule: got %q want sensitive_path", a.Rule)
	}
	if a.Severity != "warn" {
		t.Errorf("severity: got %q want warn", a.Severity)
	}
}

func TestSentinel_SensitivePath_IgnoresUnrelatedPayloads(t *testing.T) {
	s := NewSentinel(Config{})
	sub := s.Subscribe(8)
	ev := trajectory.Event{
		SessionID: "sess-1",
		Kind:      trajectory.SubtaskToolCall,
		Payload:   json.RawMessage(`{"path":"/tmp/normal.txt"}`),
	}
	s.process(ev)
	expectNoAlert(t, sub, 50*time.Millisecond)
}

func TestSentinel_Watch_BudgetTriggersCancelViaTicker(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	bus := trajectory.NewBus()
	defer bus.Shutdown()

	s := NewSentinel(Config{
		BudgetWallClock: 100 * time.Millisecond,
		Cancel: func() {
			select {
			case cancelled <- struct{}{}:
			default:
			}
		},
	})
	sub := s.Subscribe(8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go s.Watch(ctx, bus)

	// Watch's ticker fires every ~1s, so the critical alert arrives within ~2s.
	select {
	case a := <-sub:
		if a.Severity != "critical" {
			t.Fatalf("expected critical, got %+v", a)
		}
		if a.Rule != "budget_exhausted" {
			t.Fatalf("rule: got %q want budget_exhausted", a.Rule)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no critical alert from Watch within 3s")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Error("cancel hook not invoked")
	}
}

func TestSentinel_Watch_NilBusIsNoOp(t *testing.T) {
	s := NewSentinel(Config{})
	// Should return immediately without panicking.
	done := make(chan struct{})
	go func() {
		s.Watch(context.Background(), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not return for nil bus")
	}
}

// mockRecorder captures alerts persisted by Sentinel.fire().
type mockRecorder struct {
	seq    int64
	events []trajectory.Event
}

func (m *mockRecorder) AllocSeq(sessionID string) int64 {
	m.seq++
	return m.seq
}
func (m *mockRecorder) PersistSync(ev trajectory.Event) {
	m.events = append(m.events, ev)
}

func TestSentinel_Fire_PersistsAndRepublishesOnBus(t *testing.T) {
	rec := &mockRecorder{}
	bus := trajectory.NewBus()
	defer bus.Shutdown()
	busCh := bus.Subscribe(8)

	s := NewSentinel(Config{
		BudgetWallClock: 10 * time.Second,
		Recorder:        rec,
		PublishOnBus:    bus,
	})
	sub := s.Subscribe(8)
	s.sessionID = "sess-1"
	s.startedAt = time.Now().Add(-time.Hour)
	s.tickBudget()

	// Subscriber on the Sentinel itself.
	a := waitAlert(t, sub)
	if a.Severity != "critical" {
		t.Errorf("severity: got %q want critical", a.Severity)
	}

	// Recorder persisted the alert as a SentinelAlert event.
	if len(rec.events) != 1 {
		t.Fatalf("recorder events: got %d want 1", len(rec.events))
	}
	if rec.events[0].Kind != trajectory.SentinelAlert {
		t.Errorf("kind: got %q want %q", rec.events[0].Kind, trajectory.SentinelAlert)
	}
	if rec.events[0].SessionID != "sess-1" {
		t.Errorf("session_id: got %q", rec.events[0].SessionID)
	}
	if rec.events[0].Seq != 1 {
		t.Errorf("seq: got %d want 1", rec.events[0].Seq)
	}

	// The same event also went out on the trajectory bus.
	select {
	case ev := <-busCh:
		if ev.Kind != trajectory.SentinelAlert {
			t.Errorf("bus kind: got %q want %q", ev.Kind, trajectory.SentinelAlert)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event republished on bus")
	}
}

func TestSentinel_Fire_NoSessionIDSkipsPersistButReachesSubscribers(t *testing.T) {
	rec := &mockRecorder{}
	bus := trajectory.NewBus()
	defer bus.Shutdown()
	busCh := bus.Subscribe(8)

	s := NewSentinel(Config{
		BudgetWallClock: 10 * time.Second,
		Recorder:        rec,
		PublishOnBus:    bus,
	})
	sub := s.Subscribe(8)
	// sessionID intentionally empty — fire() should still notify subscribers
	// but skip recorder + bus republish.
	s.startedAt = time.Now().Add(-time.Hour)
	s.tickBudget()

	a := waitAlert(t, sub)
	if a.Severity != "critical" {
		t.Errorf("severity: got %q want critical", a.Severity)
	}
	if len(rec.events) != 0 {
		t.Errorf("recorder events: got %d want 0", len(rec.events))
	}
	select {
	case ev := <-busCh:
		t.Fatalf("unexpected bus event without session: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSentinel_Process_BindsSessionIDFromFirstEvent(t *testing.T) {
	s := NewSentinel(Config{})
	s.process(trajectory.Event{
		SessionID: "sess-xyz",
		Kind:      trajectory.SubtaskCompleted,
		Payload:   json.RawMessage(`{}`),
	})
	if s.sessionID != "sess-xyz" {
		t.Errorf("sessionID: got %q want sess-xyz", s.sessionID)
	}
	// Second event with a different session must not overwrite.
	s.process(trajectory.Event{
		SessionID: "sess-other",
		Kind:      trajectory.SubtaskCompleted,
		Payload:   json.RawMessage(`{}`),
	})
	if s.sessionID != "sess-xyz" {
		t.Errorf("sessionID got overwritten: %q", s.sessionID)
	}
}

func TestIsErrorEvent(t *testing.T) {
	errs := []trajectory.Kind{
		trajectory.SubtaskFailed,
		trajectory.GateFailed,
		trajectory.ToolFailed,
		trajectory.PlanFallback,
		trajectory.RunFailed,
	}
	for _, k := range errs {
		if !isErrorEvent(k) {
			t.Errorf("isErrorEvent(%q) = false, want true", k)
		}
	}
	nonErrs := []trajectory.Kind{
		trajectory.SubtaskCompleted,
		trajectory.SubtaskStarted,
		trajectory.GatePassed,
		trajectory.RunCompleted,
	}
	for _, k := range nonErrs {
		if isErrorEvent(k) {
			t.Errorf("isErrorEvent(%q) = true, want false", k)
		}
	}
}
