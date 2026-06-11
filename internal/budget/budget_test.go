package budget

import (
	"errors"
	"testing"
)

func TestBudget_NoCaps_NeverTrips(t *testing.T) {
	var b Budget
	if err := b.CheckPreCall(); err != nil {
		t.Fatalf("uncapped: pre-call check should not trip, got %v", err)
	}
	warn, exc := b.AddOutcome(Usage{TokensIn: 1000, TokensOut: 2000, ApproxUSDCents: 500})
	if warn != nil {
		t.Errorf("uncapped: no warning expected, got %+v", warn)
	}
	if exc != nil {
		t.Errorf("uncapped: no cap-exceeded expected, got %v", exc)
	}
}

func TestBudget_TokensCap_TripsAfterCumulative(t *testing.T) {
	b := &Budget{MaxTokens: 100}
	// First call: 85 tokens — should warn (crosses 80%) but not fail.
	warn, exc := b.AddOutcome(Usage{TokensIn: 45, TokensOut: 40})
	if exc != nil {
		t.Fatalf("first call should not exhaust, got %v", exc)
	}
	if warn == nil || warn.Kind != "tokens" {
		t.Fatalf("first call: expected tokens warning at 80%% crossing, got %+v", warn)
	}
	// Second call: pushes total to 110 — should exceed.
	_, exc = b.AddOutcome(Usage{TokensIn: 15, TokensOut: 10})
	if exc == nil {
		t.Fatalf("second call should exhaust the budget, got nil")
	}
	if !errors.Is(exc, ErrBudgetExceeded) {
		t.Fatalf("exc should wrap ErrBudgetExceeded, got %v", exc)
	}
	info, ok := InfoFromError(exc)
	if !ok || info.Kind != "tokens" || info.Cap != 100 {
		t.Fatalf("InfoFromError: got %+v ok=%v", info, ok)
	}
	// Pre-call now refuses further calls.
	if err := b.CheckPreCall(); err == nil {
		t.Fatalf("post-exhaustion CheckPreCall should refuse, got nil")
	}
}

func TestBudget_USDCap_TripsCleanly(t *testing.T) {
	// $0.10 cap; first call charges $0.06, second call charges $0.05 → trip.
	b := &Budget{MaxUSDCents: 10}
	_, exc := b.AddOutcome(Usage{ApproxUSDCents: 6})
	if exc != nil {
		t.Fatalf("first call should fit under $0.10, got %v", exc)
	}
	_, exc = b.AddOutcome(Usage{ApproxUSDCents: 5})
	if exc == nil {
		t.Fatalf("second call should exhaust usd cap, got nil")
	}
	if !errors.Is(exc, ErrBudgetExceeded) {
		t.Fatalf("usd cap error should wrap ErrBudgetExceeded, got %v", exc)
	}
	info, _ := InfoFromError(exc)
	if info.Kind != "usd_cents" {
		t.Fatalf("usd cap info kind: got %q want usd_cents", info.Kind)
	}
}

func TestBudget_PerCallCap_TripsOnOversizedCall(t *testing.T) {
	b := &Budget{PerCallMaxTokens: 100}
	_, exc := b.AddOutcome(Usage{TokensIn: 80, TokensOut: 30}) // 110 > 100
	if exc == nil {
		t.Fatalf("oversized single call should trip per-call cap")
	}
	info, _ := InfoFromError(exc)
	if info.Kind != "per_call_tokens" {
		t.Fatalf("per-call info kind: got %q want per_call_tokens", info.Kind)
	}
	// Latching: once any cap has tripped, CheckPreCall must refuse
	// subsequent calls so the supervisor's post-dispatch sweep notices
	// even though the per-call dimension itself does not accumulate.
	if err := b.CheckPreCall(); err == nil {
		t.Fatalf("after per-call trip, CheckPreCall should latch and refuse, got nil")
	} else if linfo, _ := InfoFromError(err); linfo.Kind != "per_call_tokens" {
		t.Fatalf("latched CheckPreCall info kind: got %q want per_call_tokens", linfo.Kind)
	}
}

func TestBudget_WarnFiresOnce(t *testing.T) {
	b := &Budget{MaxTokens: 100}
	// Cross the 80% threshold with the first call.
	warn, _ := b.AddOutcome(Usage{TokensIn: 80})
	if warn == nil {
		t.Fatalf("first call across 80%% should produce a warn signal")
	}
	// Subsequent sub-threshold call must not re-fire the same warning.
	warn, _ = b.AddOutcome(Usage{TokensIn: 5})
	if warn != nil {
		t.Errorf("warn should fire at most once per dimension, got %+v", warn)
	}
}

func TestInfoFromError_UnrelatedError(t *testing.T) {
	if _, ok := InfoFromError(errors.New("nope")); ok {
		t.Fatalf("InfoFromError should report ok=false for non-budget errors")
	}
	if _, ok := InfoFromError(nil); ok {
		t.Fatalf("InfoFromError(nil) should report ok=false")
	}
}
