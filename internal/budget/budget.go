// Package budget bounds an orchestration run's API spend. It tracks
// cumulative tokens and dollar-cents across every provider call in a
// session and trips a sentinel error once a configured cap is exceeded.
//
// The budget itself is library-shaped: callers (typically engine.Supervisor)
// add provider usage to a Budget after each call and ask Check() whether
// the next call should be allowed. The supervisor is responsible for
// emitting trajectory events and aborting the run — this package owns
// the accounting and the sentinel error, nothing else.
//
// The existing wall-clock budget (the Sentinel's BudgetWallClock and the
// supervisor's MaxWallSeconds timer) is intentionally separate: it
// enforces a real-time ceiling on a run, while this package enforces a
// resource-cost ceiling. Both can be active on the same run.
package budget

import (
	"errors"
	"fmt"
	"sync"
)

// ErrBudgetExceeded is wrapped by every error returned when a hard cap
// trips. Callers use errors.Is to detect it.
var ErrBudgetExceeded = errors.New("budget exceeded")

// Budget is the per-run resource ceiling. Zero values mean "no cap" for
// that dimension. The struct is mutable: callers Add() usage after each
// provider call and the budget keeps a running total.
type Budget struct {
	// MaxTokens is the cumulative cap on input+output tokens across every
	// provider call in the run. 0 disables the cap.
	MaxTokens int64

	// MaxUSDCents is the cumulative cap on dollar spend across every
	// provider call in the run, expressed in U.S. cents (1 = $0.01).
	// Cents (not floats) keep the cap check exact. 0 disables the cap.
	MaxUSDCents int64

	// PerCallMaxTokens is a per-call ceiling on input+output tokens.
	// 0 disables the cap. Enforced as a post-hoc check after the call
	// completes — providers do not pre-declare expected usage.
	PerCallMaxTokens int64

	mu        sync.Mutex
	tokens    int64
	usdCents  int64
	warned80T bool
	warned80U bool
	// tripped latches the first cap-exceeded event seen on this budget.
	// The per-call cap is detected once and discarded by AddOutcome —
	// without latching, a post-dispatch check has no way to tell that
	// the run hit a hard ceiling on an individual call. Cumulative caps
	// are also latched for symmetry.
	tripped *exceededError
}

// Usage is the unit-of-account this package operates on: one provider
// call's reported (or estimated) token + dollar usage.
type Usage struct {
	TokensIn       int64
	TokensOut      int64
	ApproxUSDCents int64
}

// TotalTokens returns the cumulative tokens recorded so far.
func (b *Budget) TotalTokens() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

// TotalUSDCents returns the cumulative dollar usage recorded so far.
func (b *Budget) TotalUSDCents() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usdCents
}

// CheckPreCall verifies, before issuing a provider call, that the budget
// has not already been exhausted by prior calls. It is the cheap pre-flight
// check: token/dollar caps that were already breached on a previous call
// stop the run before any new request is sent. A previously-latched
// per-call cap trip also returns here so the post-dispatch sweep in the
// supervisor can detect it even though the per-call dimension does not
// accumulate.
//
// Returns nil when the call may proceed, or an error wrapping
// ErrBudgetExceeded describing the dimension that has been exceeded.
func (b *Budget) CheckPreCall() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tripped != nil {
		return b.tripped
	}
	if b.MaxTokens > 0 && b.tokens >= b.MaxTokens {
		return &exceededError{kind: "tokens", used: b.tokens, cap: b.MaxTokens}
	}
	if b.MaxUSDCents > 0 && b.usdCents >= b.MaxUSDCents {
		return &exceededError{kind: "usd_cents", used: b.usdCents, cap: b.MaxUSDCents}
	}
	return nil
}

// AddOutcome records a call's reported usage. Returns:
//   - warn: a one-time-per-dimension WarnSignal when the running total
//     crosses 80% of a cap;
//   - exceeded: a non-nil error wrapping ErrBudgetExceeded when any cap
//     has now been reached (including the per-call cap).
//
// AddOutcome accumulates unconditionally — the totals reflect work the
// provider already performed, so they must be charged to the budget even
// when the caller will subsequently abort the run.
func (b *Budget) AddOutcome(u Usage) (warn *WarnSignal, exceeded error) {
	if b == nil {
		return nil, nil
	}
	thisCall := u.TokensIn + u.TokensOut
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += thisCall
	b.usdCents += u.ApproxUSDCents

	if b.PerCallMaxTokens > 0 && thisCall > b.PerCallMaxTokens {
		exceeded = &exceededError{kind: "per_call_tokens", used: thisCall, cap: b.PerCallMaxTokens}
	}
	if exceeded == nil && b.MaxTokens > 0 && b.tokens >= b.MaxTokens {
		exceeded = &exceededError{kind: "tokens", used: b.tokens, cap: b.MaxTokens}
	}
	if exceeded == nil && b.MaxUSDCents > 0 && b.usdCents >= b.MaxUSDCents {
		exceeded = &exceededError{kind: "usd_cents", used: b.usdCents, cap: b.MaxUSDCents}
	}
	if exceeded != nil && b.tripped == nil {
		if ee, ok := exceeded.(*exceededError); ok {
			b.tripped = ee
		}
	}

	if !b.warned80T && b.MaxTokens > 0 && b.tokens*10 >= b.MaxTokens*8 && b.tokens < b.MaxTokens {
		b.warned80T = true
		warn = &WarnSignal{Kind: "tokens", Used: b.tokens, Cap: b.MaxTokens}
	}
	if warn == nil && !b.warned80U && b.MaxUSDCents > 0 && b.usdCents*10 >= b.MaxUSDCents*8 && b.usdCents < b.MaxUSDCents {
		b.warned80U = true
		warn = &WarnSignal{Kind: "usd_cents", Used: b.usdCents, Cap: b.MaxUSDCents}
	}
	return warn, exceeded
}

// WarnSignal is emitted by AddOutcome at most once per dimension when the
// running total crosses 80% of a cap. The supervisor surfaces it as a
// trajectory budget_warning event.
type WarnSignal struct {
	Kind string // "tokens" | "usd_cents"
	Used int64
	Cap  int64
}

// ExceededInfo identifies which cap tripped and the offending values.
// Callers extract it via InfoFromError to fill trajectory payloads.
type ExceededInfo struct {
	Kind string // "tokens" | "usd_cents" | "per_call_tokens"
	Used int64
	Cap  int64
}

type exceededError struct {
	kind string
	used int64
	cap  int64
}

func (e *exceededError) Error() string {
	return fmt.Sprintf("%s budget exceeded (used=%d, cap=%d): %s", e.kind, e.used, e.cap, ErrBudgetExceeded.Error())
}

func (e *exceededError) Unwrap() error { return ErrBudgetExceeded }

func (e *exceededError) Info() ExceededInfo {
	return ExceededInfo{Kind: e.kind, Used: e.used, Cap: e.cap}
}

// InfoFromError unwraps an error returned by Budget and returns the
// ExceededInfo when one is present. ok=false when the error did not come
// from this package.
func InfoFromError(err error) (ExceededInfo, bool) {
	for e := err; e != nil; {
		if ee, ok := e.(*exceededError); ok {
			return ee.Info(), true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return ExceededInfo{}, false
		}
		e = u.Unwrap()
	}
	return ExceededInfo{}, false
}
